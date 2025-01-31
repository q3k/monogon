// cluster builds on the launch package and implements launching Metropolis
// nodes and clusters in a virtualized environment using qemu. It's kept in a
// separate package as it depends on a Metropolis node image, which might not be
// required for some use of the launch library.
package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"go.uber.org/multierr"
	"golang.org/x/net/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"source.monogon.dev/metropolis/cli/pkg/datafile"
	"source.monogon.dev/metropolis/node"
	"source.monogon.dev/metropolis/node/core/identity"
	"source.monogon.dev/metropolis/node/core/rpc"
	"source.monogon.dev/metropolis/node/core/rpc/resolver"
	apb "source.monogon.dev/metropolis/proto/api"
	cpb "source.monogon.dev/metropolis/proto/common"
	"source.monogon.dev/metropolis/test/launch"
)

// NodeOptions contains all options that can be passed to Launch()
type NodeOptions struct {
	// Name is a human-readable identifier to be used in debug output.
	Name string

	// Ports contains the port mapping where to expose the internal ports of the VM to
	// the host. See IdentityPortMap() and ConflictFreePortMap(). Ignored when
	// ConnectToSocket is set.
	Ports launch.PortMap

	// If set to true, reboots are honored. Otherwise, all reboots exit the Launch()
	// command. Metropolis nodes generally restart on almost all errors, so unless you
	// want to test reboot behavior this should be false.
	AllowReboot bool

	// By default, the VM is connected to the Host via SLIRP. If ConnectToSocket is
	// set, it is instead connected to the given file descriptor/socket. If this is
	// set, all port maps from the Ports option are ignored. Intended for networking
	// this instance together with others for running more complex network
	// configurations.
	ConnectToSocket *os.File

	// When PcapDump is set, all traffic is dumped to a pcap file in the
	// runtime directory (e.g. "net0.pcap" for the first interface).
	PcapDump bool

	// SerialPort is an io.ReadWriter over which you can communicate with the serial
	// port of the machine. It can be set to an existing file descriptor (like
	// os.Stdout/os.Stderr) or any Go structure implementing this interface.
	SerialPort io.ReadWriter

	// NodeParameters is passed into the VM and subsequently used for bootstrapping or
	// registering into a cluster.
	NodeParameters *apb.NodeParameters

	// Mac is the node's MAC address.
	Mac *net.HardwareAddr

	// Runtime keeps the node's QEMU runtime state.
	Runtime *NodeRuntime
}

// NodeRuntime keeps the node's QEMU runtime options.
type NodeRuntime struct {
	// ld points at the node's launch directory storing data such as storage
	// images, firmware variables or the TPM state.
	ld string
	// sd points at the node's socket directory.
	sd string

	// ctxT is the context QEMU will execute in.
	ctxT context.Context
	// CtxC is the QEMU context's cancellation function.
	CtxC context.CancelFunc
}

// NodePorts is the list of ports a fully operational Metropolis node listens on
var NodePorts = []node.Port{
	node.ConsensusPort,

	node.CuratorServicePort,
	node.DebugServicePort,

	node.KubernetesAPIPort,
	node.KubernetesAPIWrappedPort,
	node.CuratorServicePort,
	node.DebuggerPort,
}

// setupRuntime creates the node's QEMU runtime directory, together with all
// files required to preserve its state, a level below the chosen path ld. The
// node's socket directory is similarily created a level below sd. It may
// return an I/O error.
func setupRuntime(ld, sd string) (*NodeRuntime, error) {
	// Create a temporary directory to keep all the runtime files.
	stdp, err := os.MkdirTemp(ld, "node_state*")
	if err != nil {
		return nil, fmt.Errorf("failed to create the state directory: %w", err)
	}

	// Initialize the node's storage with a prebuilt image.
	si, err := datafile.ResolveRunfile("metropolis/node/node.img")
	if err != nil {
		return nil, fmt.Errorf("while resolving a path: %w", err)
	}
	di := filepath.Join(stdp, filepath.Base(si))
	log.Printf("Cluster: copying the node image: %s -> %s", si, di)
	if err := copyFile(si, di); err != nil {
		return nil, fmt.Errorf("while copying the node image: %w", err)
	}

	// Initialize the OVMF firmware variables file.
	sv, err := datafile.ResolveRunfile("external/edk2/OVMF_VARS.fd")
	if err != nil {
		return nil, fmt.Errorf("while resolving a path: %w", err)
	}
	dv := filepath.Join(stdp, filepath.Base(sv))
	if err := copyFile(sv, dv); err != nil {
		return nil, fmt.Errorf("while copying firmware variables: %w", err)
	}

	// Create the TPM state directory and initialize all files required by swtpm.
	tpmt := filepath.Join(stdp, "tpm")
	if err := os.Mkdir(tpmt, 0755); err != nil {
		return nil, fmt.Errorf("while creating the TPM directory: %w", err)
	}
	tpms, err := datafile.ResolveRunfile("metropolis/node/tpm")
	if err != nil {
		return nil, fmt.Errorf("while resolving a path: %w", err)
	}
	tpmf, err := os.ReadDir(tpms)
	if err != nil {
		return nil, fmt.Errorf("failed to read TPM directory: %w", err)
	}
	for _, file := range tpmf {
		name := file.Name()
		src, err := datafile.ResolveRunfile(filepath.Join(tpms, name))
		if err != nil {
			return nil, fmt.Errorf("while resolving a path: %w", err)
		}
		tgt := filepath.Join(tpmt, name)
		if err := copyFile(src, tgt); err != nil {
			return nil, fmt.Errorf("while copying TPM state: file %q to %q: %w", src, tgt, err)
		}
	}

	// Create the socket directory.
	sotdp, err := os.MkdirTemp(sd, "node_sock*")
	if err != nil {
		return nil, fmt.Errorf("failed to create the socket directory: %w", err)
	}

	return &NodeRuntime{
		ld: stdp,
		sd: sotdp,
	}, nil
}

// CuratorClient returns an authenticated owner connection to a Curator
// instance within Cluster c, or nil together with an error.
func (c *Cluster) CuratorClient() (*grpc.ClientConn, error) {
	if c.authClient == nil {
		authCreds := rpc.NewAuthenticatedCredentials(c.Owner, nil)
		r := resolver.New(c.ctxT, resolver.WithLogger(func(f string, args ...interface{}) {
			log.Printf("Cluster: client resolver: %s", fmt.Sprintf(f, args...))
		}))
		for _, n := range c.NodeIDs {
			ep, err := resolver.NodeWithDefaultPort(n)
			if err != nil {
				return nil, fmt.Errorf("could not add node %q by DNS: %v", n, err)
			}
			r.AddEndpoint(ep)
		}
		authClient, err := grpc.Dial(resolver.MetropolisControlAddress,
			grpc.WithTransportCredentials(authCreds),
			grpc.WithResolvers(r),
			grpc.WithContextDialer(c.DialNode),
		)
		if err != nil {
			return nil, fmt.Errorf("dialing with owner credentials failed: %w", err)
		}
		c.authClient = authClient
	}
	return c.authClient, nil
}

// LaunchNode launches a single Metropolis node instance with the given options.
// The instance runs mostly paravirtualized but with some emulated hardware
// similar to how a cloud provider might set up its VMs. The disk is fully
// writable, and the changes are kept across reboots and shutdowns. ld and sd
// point to the launch directory and the socket directory, holding the nodes'
// state files (storage, tpm state, firmware state), and UNIX socket files
// (swtpm <-> QEMU interplay) respectively. The directories must exist before
// LaunchNode is called. LaunchNode will update options.Runtime and options.Mac
// if either are not initialized.
func LaunchNode(ctx context.Context, ld, sd string, options *NodeOptions) error {
	// TODO(mateusz@monogon.tech) try using QEMU's abstract socket namespace instead
	// of /tmp (requires QEMU version >5.0).
	// https://github.com/qemu/qemu/commit/776b97d3605ed0fc94443048fdf988c7725e38a9).
	// swtpm accepts already-open FDs so we can pass in an abstract socket namespace FD
	// that we open and pass the name of it to QEMU. Not pinning this crashes both
	// swtpm and qemu because we run into UNIX socket length limitations (for legacy
	// reasons 108 chars).

	// If it's the node's first start, set up its runtime directories.
	if options.Runtime == nil {
		r, err := setupRuntime(ld, sd)
		if err != nil {
			return fmt.Errorf("while setting up node runtime: %w", err)
		}
		options.Runtime = r
	}

	// Replace the node's context with a new one.
	r := options.Runtime
	if r.CtxC != nil {
		r.CtxC()
	}
	r.ctxT, r.CtxC = context.WithCancel(ctx)

	var qemuNetType string
	var qemuNetConfig launch.QemuValue
	if options.ConnectToSocket != nil {
		qemuNetType = "socket"
		qemuNetConfig = launch.QemuValue{
			"id": {"net0"},
			"fd": {"3"},
		}
	} else {
		qemuNetType = "user"
		qemuNetConfig = launch.QemuValue{
			"id":        {"net0"},
			"net":       {"10.42.0.0/24"},
			"dhcpstart": {"10.42.0.10"},
			"hostfwd":   options.Ports.ToQemuForwards(),
		}
	}

	// Generate the node's MAC address if it isn't already set in NodeOptions.
	if options.Mac == nil {
		mac, err := generateRandomEthernetMAC()
		if err != nil {
			return err
		}
		options.Mac = mac
	}

	tpmSocketPath := filepath.Join(r.sd, "tpm-socket")
	fwVarPath := filepath.Join(r.ld, "OVMF_VARS.fd")
	storagePath := filepath.Join(r.ld, "node.img")
	qemuArgs := []string{"-machine", "q35", "-accel", "kvm", "-nographic", "-nodefaults", "-m", "4096",
		"-cpu", "host", "-smp", "sockets=1,cpus=1,cores=2,threads=2,maxcpus=4",
		"-drive", "if=pflash,format=raw,readonly,file=external/edk2/OVMF_CODE.fd",
		"-drive", "if=pflash,format=raw,file=" + fwVarPath,
		"-drive", "if=virtio,format=raw,cache=unsafe,file=" + storagePath,
		"-netdev", qemuNetConfig.ToOption(qemuNetType),
		"-device", "virtio-net-pci,netdev=net0,mac=" + options.Mac.String(),
		"-chardev", "socket,id=chrtpm,path=" + tpmSocketPath,
		"-tpmdev", "emulator,id=tpm0,chardev=chrtpm",
		"-device", "tpm-tis,tpmdev=tpm0",
		"-device", "virtio-rng-pci",
		"-serial", "stdio"}

	if !options.AllowReboot {
		qemuArgs = append(qemuArgs, "-no-reboot")
	}

	if options.NodeParameters != nil {
		parametersPath := filepath.Join(r.ld, "parameters.pb")
		parametersRaw, err := proto.Marshal(options.NodeParameters)
		if err != nil {
			return fmt.Errorf("failed to encode node paraeters: %w", err)
		}
		if err := os.WriteFile(parametersPath, parametersRaw, 0644); err != nil {
			return fmt.Errorf("failed to write node parameters: %w", err)
		}
		qemuArgs = append(qemuArgs, "-fw_cfg", "name=dev.monogon.metropolis/parameters.pb,file="+parametersPath)
	}

	if options.PcapDump {
		var qemuNetDump launch.QemuValue
		pcapPath := filepath.Join(r.ld, "net0.pcap")
		if options.PcapDump {
			qemuNetDump = launch.QemuValue{
				"id":     {"net0"},
				"netdev": {"net0"},
				"file":   {pcapPath},
			}
		}
		qemuArgs = append(qemuArgs, "-object", qemuNetDump.ToOption("filter-dump"))
	}

	// Start TPM emulator as a subprocess
	tpmCtx, tpmCancel := context.WithCancel(options.Runtime.ctxT)
	defer tpmCancel()

	tpmd := filepath.Join(r.ld, "tpm")
	tpmEmuCmd := exec.CommandContext(tpmCtx, "swtpm", "socket", "--tpm2", "--tpmstate", "dir="+tpmd, "--ctrl", "type=unixio,path="+tpmSocketPath)
	tpmEmuCmd.Stderr = os.Stderr
	tpmEmuCmd.Stdout = os.Stdout

	err := tpmEmuCmd.Start()
	if err != nil {
		return fmt.Errorf("failed to start TPM emulator: %w", err)
	}

	// Wait for the socket to be created by the TPM emulator before launching
	// QEMU.
	for {
		_, err := os.Stat(tpmSocketPath)
		if err == nil {
			break
		}
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("while stat-ing TPM socket path: %w", err)
		}
		if err := tpmCtx.Err(); err != nil {
			return fmt.Errorf("while waiting for the TPM socket: %w", err)
		}
		time.Sleep(time.Millisecond * 100)
	}

	// Start the main qemu binary
	systemCmd := exec.CommandContext(options.Runtime.ctxT, "qemu-system-x86_64", qemuArgs...)
	if options.ConnectToSocket != nil {
		systemCmd.ExtraFiles = []*os.File{options.ConnectToSocket}
	}

	var stdErrBuf bytes.Buffer
	systemCmd.Stderr = &stdErrBuf
	systemCmd.Stdout = options.SerialPort

	launch.PrettyPrintQemuArgs(options.Name, systemCmd.Args)

	err = systemCmd.Run()

	// Stop TPM emulator and wait for it to exit to properly reap the child process
	tpmCancel()
	log.Print("Node: Waiting for TPM emulator to exit")
	// Wait returns a SIGKILL error because we just cancelled its context.
	// We still need to call it to avoid creating zombies.
	_ = tpmEmuCmd.Wait()
	log.Print("Node: TPM emulator done")

	var exerr *exec.ExitError
	if err != nil && errors.As(err, &exerr) {
		status := exerr.ProcessState.Sys().(syscall.WaitStatus)
		if status.Signaled() && status.Signal() == syscall.SIGKILL {
			// Process was killed externally (most likely by our context being canceled).
			// This is a normal exit for us, so return nil
			return nil
		}
		exerr.Stderr = stdErrBuf.Bytes()
		newErr := launch.QEMUError(*exerr)
		return &newErr
	}
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("when opening source: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("when creating destination: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return fmt.Errorf("when copying file: %w", err)
	}
	return out.Close()
}

// getNodes wraps around Management.GetNodes to return a list of nodes in a
// cluster.
func getNodes(ctx context.Context, mgmt apb.ManagementClient) ([]*apb.Node, error) {
	var res []*apb.Node
	bo := backoff.WithContext(backoff.NewExponentialBackOff(), ctx)
	err := backoff.Retry(func() error {
		res = nil
		srvN, err := mgmt.GetNodes(ctx, &apb.GetNodesRequest{})
		if err != nil {
			return fmt.Errorf("GetNodes: %w", err)
		}
		for {
			node, err := srvN.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("GetNodes.Recv: %w", err)
			}
			res = append(res, node)
		}
		return nil
	}, bo)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// getNode wraps Management.GetNodes. It returns node information matching
// given node ID.
func getNode(ctx context.Context, mgmt apb.ManagementClient, id string) (*apb.Node, error) {
	nodes, err := getNodes(ctx, mgmt)
	if err != nil {
		return nil, fmt.Errorf("could not get nodes: %w", err)
	}
	for _, n := range nodes {
		eid := identity.NodeID(n.Pubkey)
		if eid != id {
			continue
		}
		return n, nil
	}
	return nil, fmt.Errorf("no such node.")
}

// Gets a random EUI-48 Ethernet MAC address
func generateRandomEthernetMAC() (*net.HardwareAddr, error) {
	macBuf := make([]byte, 6)
	_, err := rand.Read(macBuf)
	if err != nil {
		return nil, fmt.Errorf("failed to read randomness for MAC: %v", err)
	}

	// Set U/L bit and clear I/G bit (locally administered individual MAC)
	// Ref IEEE 802-2014 Section 8.2.2
	macBuf[0] = (macBuf[0] | 2) & 0xfe
	mac := net.HardwareAddr(macBuf)
	return &mac, nil
}

const SOCKSPort uint16 = 1080

// ClusterPorts contains all ports handled by Nanoswitch.
var ClusterPorts = []uint16{
	// Forwarded to the first node.
	uint16(node.CuratorServicePort),
	uint16(node.DebugServicePort),
	uint16(node.KubernetesAPIPort),
	uint16(node.KubernetesAPIWrappedPort),

	// SOCKS proxy to the switch network
	SOCKSPort,
}

// ClusterOptions contains all options for launching a Metropolis cluster.
type ClusterOptions struct {
	// The number of nodes this cluster should be started with.
	NumNodes int
}

// Cluster is the running Metropolis cluster launched using the LaunchCluster
// function.
type Cluster struct {
	// Owner is the TLS Certificate of the owner of the test cluster. This can be
	// used to authenticate further clients to the running cluster.
	Owner tls.Certificate
	// Ports is the PortMap used to access the first nodes' services (defined in
	// ClusterPorts) and the SOCKS proxy (at SOCKSPort).
	Ports launch.PortMap

	// Nodes is a map from Node ID to its runtime information.
	Nodes map[string]*NodeInCluster
	// NodeIDs is a list of node IDs that are backing this cluster, in order of
	// creation.
	NodeIDs []string

	// nodesDone is a list of channels populated with the return codes from all the
	// nodes' qemu instances. It's used by Close to ensure all nodes have
	// successfully been stopped.
	nodesDone []chan error
	// nodeOpts are the cluster member nodes' mutable launch options, kept here
	// to facilitate reboots.
	nodeOpts []NodeOptions
	// launchDir points at the directory keeping the nodes' state, such as storage
	// images, firmware variable files, TPM state.
	launchDir string
	// socketDir points at the directory keeping UNIX socket files, such as these
	// used to facilitate communication between QEMU and swtpm. It's different
	// from launchDir, and anchored nearer the file system root, due to the
	// socket path length limitation imposed by the kernel.
	socketDir string

	// socksDialer is used by DialNode to establish connections to nodes via the
	// SOCKS server ran by nanoswitch.
	socksDialer proxy.Dialer

	// authClient is a cached authenticated owner connection to a Curator
	// instance within the cluster.
	authClient *grpc.ClientConn

	// ctxT is the context individual node contexts are created from.
	ctxT context.Context
	// ctxC is used by Close to cancel the context under which the nodes are
	// running.
	ctxC context.CancelFunc
}

// NodeInCluster represents information about a node that's part of a Cluster.
type NodeInCluster struct {
	// ID of the node, which can be used to dial this node's services via DialNode.
	ID string
	// Address of the node on the network ran by nanoswitch. Not reachable from the
	// host unless dialed via DialNode or via the nanoswitch SOCKS proxy (reachable
	// on Cluster.Ports[SOCKSPort]).
	ManagementAddress string
}

// firstConnection performs the initial owner credential escrow with a newly
// started nanoswitch-backed cluster over SOCKS. It expects the first node to be
// running at 10.1.0.2, which is always the case with the current nanoswitch
// implementation.
//
// It returns the newly escrowed credentials as well as the first node's
// information as NodeInCluster.
func firstConnection(ctx context.Context, socksDialer proxy.Dialer) (*tls.Certificate, *NodeInCluster, error) {
	// Dial external service.
	remote := fmt.Sprintf("10.1.0.2:%s", node.CuratorServicePort.PortString())
	initCreds, err := rpc.NewEphemeralCredentials(InsecurePrivateKey, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("NewEphemeralCredentials: %w", err)
	}
	initDialer := func(_ context.Context, addr string) (net.Conn, error) {
		return socksDialer.Dial("tcp", addr)
	}
	initClient, err := grpc.Dial(remote, grpc.WithContextDialer(initDialer), grpc.WithTransportCredentials(initCreds))
	if err != nil {
		return nil, nil, fmt.Errorf("dialing with ephemeral credentials failed: %w", err)
	}
	defer initClient.Close()

	// Retrieve owner certificate - this can take a while because the node is still
	// coming up, so do it in a backoff loop.
	log.Printf("Cluster: retrieving owner certificate (this can take a few seconds while the first node boots)...")
	aaa := apb.NewAAAClient(initClient)
	var cert *tls.Certificate
	err = backoff.Retry(func() error {
		cert, err = rpc.RetrieveOwnerCertificate(ctx, aaa, InsecurePrivateKey)
		if st, ok := status.FromError(err); ok {
			if st.Code() == codes.Unavailable {
				log.Printf("Cluster: cluster UNAVAILABLE: %v", st.Message())
				return err
			}
		}
		return backoff.Permanent(err)
	}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))
	if err != nil {
		return nil, nil, fmt.Errorf("couldn't retrieve owner certificate: %w", err)
	}
	log.Printf("Cluster: retrieved owner certificate.")

	// Now connect authenticated and get the node ID.
	creds := rpc.NewAuthenticatedCredentials(*cert, nil)
	authClient, err := grpc.Dial(remote, grpc.WithContextDialer(initDialer), grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, nil, fmt.Errorf("dialing with owner credentials failed: %w", err)
	}
	defer authClient.Close()
	mgmt := apb.NewManagementClient(authClient)

	var node *NodeInCluster
	err = backoff.Retry(func() error {
		nodes, err := getNodes(ctx, mgmt)
		if err != nil {
			return fmt.Errorf("retrieving nodes failed: %w", err)
		}
		if len(nodes) != 1 {
			return fmt.Errorf("expected one node, got %d", len(nodes))
		}
		n := nodes[0]
		if n.Status == nil || n.Status.ExternalAddress == "" {
			return fmt.Errorf("node has no status and/or address")
		}
		node = &NodeInCluster{
			ID:                identity.NodeID(n.Pubkey),
			ManagementAddress: n.Status.ExternalAddress,
		}
		return nil
	}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))
	if err != nil {
		return nil, nil, err
	}

	return cert, node, nil
}

// LaunchCluster launches a cluster of Metropolis node VMs together with a
// Nanoswitch instance to network them all together.
//
// The given context will be used to run all qemu instances in the cluster, and
// canceling the context or calling Close() will terminate them.
func LaunchCluster(ctx context.Context, opts ClusterOptions) (*Cluster, error) {
	if opts.NumNodes <= 0 {
		return nil, errors.New("refusing to start cluster with zero nodes")
	}

	// Create the launch directory.
	ld, err := os.MkdirTemp(os.Getenv("TEST_TMPDIR"), "cluster*")
	if err != nil {
		return nil, fmt.Errorf("failed to create the launch directory: %w", err)
	}
	// Create the socket directory.
	sd, err := os.MkdirTemp("/tmp", "cluster*")
	if err != nil {
		return nil, fmt.Errorf("failed to create the socket directory: %w", err)
	}

	// Prepare links between nodes and nanoswitch.
	var switchPorts []*os.File
	var vmPorts []*os.File
	for i := 0; i < opts.NumNodes; i++ {
		switchPort, vmPort, err := launch.NewSocketPair()
		if err != nil {
			return nil, fmt.Errorf("failed to get socketpair: %w", err)
		}
		switchPorts = append(switchPorts, switchPort)
		vmPorts = append(vmPorts, vmPort)
	}

	// Make a list of channels that will be populated by all running node qemu
	// processes.
	done := make([]chan error, opts.NumNodes)
	for i, _ := range done {
		done[i] = make(chan error, 1)
	}

	// Prepare the node options. These will be kept as part of Cluster.
	// nodeOpts[].Runtime will be initialized by LaunchNode during the first
	// launch. The runtime information can be later used to restart a node.
	// The 0th node will be initialized first. The rest will follow after it
	// had bootstrapped the cluster.
	nodeOpts := make([]NodeOptions, opts.NumNodes)
	nodeOpts[0] = NodeOptions{
		Name:            "node0",
		ConnectToSocket: vmPorts[0],
		NodeParameters: &apb.NodeParameters{
			Cluster: &apb.NodeParameters_ClusterBootstrap_{
				ClusterBootstrap: &apb.NodeParameters_ClusterBootstrap{
					OwnerPublicKey: InsecurePublicKey,
				},
			},
		},
		SerialPort: newPrefixedStdio(0),
		PcapDump:   true,
	}

	// Start the first node.
	ctxT, ctxC := context.WithCancel(ctx)
	log.Printf("Cluster: Starting node %d...", 1)
	go func() {
		err := LaunchNode(ctxT, ld, sd, &nodeOpts[0])
		if err != nil {
			log.Printf("Node %d finished with an error: %v", 1, err)
		}
		done[0] <- err
	}()

	// Launch nanoswitch.
	portMap, err := launch.ConflictFreePortMap(ClusterPorts)
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("failed to allocate ephemeral ports: %w", err)
	}

	go func() {
		if err := launch.RunMicroVM(ctxT, &launch.MicroVMOptions{
			Name:                   "nanoswitch [99]",
			KernelPath:             "metropolis/test/ktest/vmlinux",
			InitramfsPath:          "metropolis/test/nanoswitch/initramfs.cpio.lz4",
			ExtraNetworkInterfaces: switchPorts,
			PortMap:                portMap,
			SerialPort:             newPrefixedStdio(99),
			PcapDump:               path.Join(ld, "nanoswitch.pcap"),
		}); err != nil {
			if !errors.Is(err, ctxT.Err()) {
				log.Fatalf("Failed to launch nanoswitch: %v", err)
			}
		}
	}()

	// Build SOCKS dialer.
	socksRemote := fmt.Sprintf("localhost:%v", portMap[SOCKSPort])
	socksDialer, err := proxy.SOCKS5("tcp", socksRemote, nil, proxy.Direct)
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("failed to build SOCKS dialer: %w", err)
	}

	// Retrieve owner credentials and first node.
	cert, firstNode, err := firstConnection(ctxT, socksDialer)
	if err != nil {
		ctxC()
		return nil, err
	}

	// Set up a partially initialized cluster instance, to be filled in in the
	// later steps.
	cluster := &Cluster{
		Owner: *cert,
		Ports: portMap,
		Nodes: map[string]*NodeInCluster{
			firstNode.ID: firstNode,
		},
		NodeIDs: []string{
			firstNode.ID,
		},

		nodesDone: done,
		nodeOpts:  nodeOpts,
		launchDir: ld,
		socketDir: sd,

		socksDialer: socksDialer,

		ctxT: ctxT,
		ctxC: ctxC,
	}

	// Now start the rest of the nodes and register them into the cluster.

	// Get an authenticated owner client within the cluster.
	curC, err := cluster.CuratorClient()
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("CuratorClient: %w", err)
	}
	mgmt := apb.NewManagementClient(curC)

	// Retrieve register ticket to register further nodes.
	log.Printf("Cluster: retrieving register ticket...")
	resT, err := mgmt.GetRegisterTicket(ctx, &apb.GetRegisterTicketRequest{})
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("GetRegisterTicket: %w", err)
	}
	ticket := resT.Ticket
	log.Printf("Cluster: retrieved register ticket (%d bytes).", len(ticket))

	// Retrieve cluster info (for directory and ca public key) to register further
	// nodes.
	resI, err := mgmt.GetClusterInfo(ctx, &apb.GetClusterInfoRequest{})
	if err != nil {
		ctxC()
		return nil, fmt.Errorf("GetClusterInfo: %w", err)
	}

	// Use the retrieved information to configure the rest of the node options.
	for i := 1; i < opts.NumNodes; i++ {
		nodeOpts[i] = NodeOptions{
			Name:            fmt.Sprintf("node%d", i),
			ConnectToSocket: vmPorts[i],
			NodeParameters: &apb.NodeParameters{
				Cluster: &apb.NodeParameters_ClusterRegister_{
					ClusterRegister: &apb.NodeParameters_ClusterRegister{
						RegisterTicket:   ticket,
						ClusterDirectory: resI.ClusterDirectory,
						CaCertificate:    resI.CaCertificate,
					},
				},
			},
			SerialPort: newPrefixedStdio(i),
		}
	}

	// Now run the rest of the nodes.
	//
	// TODO(q3k): parallelize this
	for i := 1; i < opts.NumNodes; i++ {
		log.Printf("Cluster: Starting node %d...", i+1)
		go func(i int) {
			err := LaunchNode(ctxT, ld, sd, &nodeOpts[i])
			if err != nil {
				log.Printf("Node %d finished with an error: %v", i, err)
			}
			done[i] <- err
		}(i)
		var newNode *apb.Node

		log.Printf("Cluster: waiting for node %d to appear as NEW...", i)
		for {
			nodes, err := getNodes(ctx, mgmt)
			if err != nil {
				ctxC()
				return nil, fmt.Errorf("could not get nodes: %w", err)
			}
			for _, n := range nodes {
				if n.State == cpb.NodeState_NODE_STATE_NEW {
					newNode = n
					break
				}
			}
			if newNode != nil {
				break
			}
			time.Sleep(1 * time.Second)
		}
		id := identity.NodeID(newNode.Pubkey)
		log.Printf("Cluster: node %d is %s", i, id)

		log.Printf("Cluster: approving node %d", i)
		_, err := mgmt.ApproveNode(ctx, &apb.ApproveNodeRequest{
			Pubkey: newNode.Pubkey,
		})
		if err != nil {
			ctxC()
			return nil, fmt.Errorf("ApproveNode(%s): %w", id, err)
		}
		log.Printf("Cluster: node %d approved, waiting for it to appear as UP and with a network address...", i)
		for {
			nodes, err := getNodes(ctx, mgmt)
			if err != nil {
				ctxC()
				return nil, fmt.Errorf("could not get nodes: %w", err)
			}
			found := false
			for _, n := range nodes {
				if !bytes.Equal(n.Pubkey, newNode.Pubkey) {
					continue
				}
				if n.Status == nil || n.Status.ExternalAddress == "" {
					break
				}
				if n.State != cpb.NodeState_NODE_STATE_UP {
					break
				}
				found = true
				cluster.Nodes[identity.NodeID(n.Pubkey)] = &NodeInCluster{
					ID:                identity.NodeID(n.Pubkey),
					ManagementAddress: n.Status.ExternalAddress,
				}
				cluster.NodeIDs = append(cluster.NodeIDs, identity.NodeID(n.Pubkey))
				break
			}
			if found {
				break
			}
			time.Sleep(time.Second)
		}
		log.Printf("Cluster: node %d (%s) UP!", i, id)
	}

	log.Printf("Cluster: all nodes up:")
	for _, node := range cluster.Nodes {
		log.Printf("Cluster:  - %s at %s", node.ID, node.ManagementAddress)
	}

	return cluster, nil
}

// RebootNode reboots the cluster member node matching the given index, and
// waits for it to rejoin the cluster. It will use the given context ctx to run
// cluster API requests, whereas the resulting QEMU process will be created
// using the cluster's context c.ctxT. The nodes are indexed starting at 0.
func (c *Cluster) RebootNode(ctx context.Context, idx int) error {
	if idx < 0 || idx >= len(c.NodeIDs) {
		return fmt.Errorf("index out of bounds.")
	}
	id := c.NodeIDs[idx]

	// Get an authenticated owner client within the cluster.
	curC, err := c.CuratorClient()
	if err != nil {
		return err
	}
	mgmt := apb.NewManagementClient(curC)

	// Get the timestamp of the node's last update, as observed by Curator.
	// It'll be needed to make sure it had rejoined the cluster after the reboot.
	var is *apb.Node
	for {
		r, err := getNode(ctx, mgmt, id)
		if err != nil {
			return err
		}

		// Node status may be absent if it hasn't reported to the cluster yet. Wait
		// for it to appear before progressing further.
		if r.Status != nil {
			is = r
			break
		}
		time.Sleep(time.Second)
	}

	// Cancel the node's context. This will shut down QEMU.
	c.nodeOpts[idx].Runtime.CtxC()
	log.Printf("Cluster: waiting for node %d (%s) to stop.", idx, id)
	err = <-c.nodesDone[idx]
	if err != nil {
		return fmt.Errorf("while restarting node: %w", err)
	}

	// Start QEMU again.
	log.Printf("Cluster: restarting node %d (%s).", idx, id)
	go func(n int) {
		err := LaunchNode(c.ctxT, c.launchDir, c.socketDir, &c.nodeOpts[n])
		if err != nil {
			log.Printf("Node %d finished with an error: %v", n, err)
		}
		c.nodesDone[n] <- err
	}(idx)

	// Poll Management.GetNodes until the node's timestamp is updated.
	for {
		cs, err := getNode(ctx, mgmt, id)
		if err != nil {
			log.Printf("Cluster: node get error: %v", err)
			return err
		}
		log.Printf("Cluster: node status: %+v", cs)
		if cs.Status == nil {
			continue
		}
		if cs.Status.Timestamp.AsTime().Sub(is.Status.Timestamp.AsTime()) > 0 {
			break
		}
		time.Sleep(time.Second)
	}
	log.Printf("Cluster: node %d (%s) has rejoined the cluster.", idx, id)
	return nil
}

// Close cancels the running clusters' context and waits for all virtualized
// nodes to stop. It returns an error if stopping the nodes failed, or one of
// the nodes failed to fully start in the first place.
func (c *Cluster) Close() error {
	log.Printf("Cluster: stopping...")
	if c.authClient != nil {
		c.authClient.Close()
	}
	c.ctxC()

	var errs []error
	log.Printf("Cluster: waiting for nodes to exit...")
	for _, c := range c.nodesDone {
		err := <-c
		if err != nil {
			errs = append(errs, err)
		}
	}
	log.Printf("Cluster: removing nodes' state files.")
	os.RemoveAll(c.launchDir)
	os.RemoveAll(c.socketDir)
	log.Printf("Cluster: done")
	return multierr.Combine(errs...)
}

// DialNode is a grpc.WithContextDialer compatible dialer which dials nodes by
// their ID. This is performed by connecting to the cluster nanoswitch via its
// SOCKS proxy, and using the cluster node list for name resolution.
//
// For example:
//
//   grpc.Dial("metropolis-deadbeef:1234", grpc.WithContextDialer(c.DialNode))
//
func (c *Cluster) DialNode(_ context.Context, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid host:port: %w", err)
	}
	// Already an IP address?
	if net.ParseIP(host) != nil {
		return c.socksDialer.Dial("tcp", addr)
	}

	// Otherwise, expect a node name.
	node, ok := c.Nodes[host]
	if !ok {
		return nil, fmt.Errorf("unknown node %q", host)
	}
	addr = net.JoinHostPort(node.ManagementAddress, port)
	return c.socksDialer.Dial("tcp", addr)
}
