// Copyright 2020 The Monogon Project Authors.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"

	"git.monogon.dev/source/nexantic.git/core/internal/common/supervisor"
	"git.monogon.dev/source/nexantic.git/core/internal/kubernetes/reconciler"
	"git.monogon.dev/source/nexantic.git/core/pkg/fileargs"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeletconfig "k8s.io/kubelet/config/v1beta1"
)

type KubeletSpec struct {
	clusterDNS []net.IP
}

func runKubelet(spec *KubeletSpec, output io.Writer) supervisor.Runnable {
	return func(ctx context.Context) error {
		fargs, err := fileargs.New()
		if err != nil {
			return err
		}
		var clusterDNS []string
		for _, dnsIP := range spec.clusterDNS {
			clusterDNS = append(clusterDNS, dnsIP.String())
		}

		kubeletConf := &kubeletconfig.KubeletConfiguration{
			TypeMeta: v1.TypeMeta{
				Kind:       "KubeletConfiguration",
				APIVersion: kubeletconfig.GroupName + "/v1beta1",
			},
			TLSCertFile:       "/data/kubernetes/kubelet.crt",
			TLSPrivateKeyFile: "/data/kubernetes/kubelet.key",
			TLSMinVersion:     "VersionTLS13",
			ClusterDNS:        clusterDNS,
			Authentication: kubeletconfig.KubeletAuthentication{
				X509: kubeletconfig.KubeletX509Authentication{
					ClientCAFile: "/data/kubernetes/ca.crt",
				},
			},
			// TODO(q3k): move reconciler.False to a generic package, fix the following references.
			ClusterDomain:                "cluster.local", // cluster.local is hardcoded in the certificate too currently
			EnableControllerAttachDetach: reconciler.False(),
			HairpinMode:                  "none",
			MakeIPTablesUtilChains:       reconciler.False(), // We don't have iptables
			FailSwapOn:                   reconciler.False(), // Our kernel doesn't have swap enabled which breaks Kubelet's detection
			KubeReserved: map[string]string{
				"cpu":    "200m",
				"memory": "300Mi",
			},

			// We're not going to use this, but let's make it point to a known-empty directory in case anybody manages to
			// trigger it.
			VolumePluginDir: "/kubernetes/conf/flexvolume-plugins",
		}

		configRaw, err := json.Marshal(kubeletConf)
		if err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "/kubernetes/bin/kube", "kubelet",
			fargs.FileOpt("--config", "config.json", configRaw),
			"--container-runtime=remote",
			"--container-runtime-endpoint=unix:///containerd/run/containerd.sock",
			"--kubeconfig=/data/kubernetes/kubelet.kubeconfig",
			"--root-dir=/data/kubernetes/kubelet",
		)
		cmd.Env = []string{"PATH=/kubernetes/bin"}
		cmd.Stdout = output
		cmd.Stderr = output

		supervisor.Signal(ctx, supervisor.SignalHealthy)
		err = cmd.Run()
		fmt.Fprintf(output, "kubelet stopped: %v\n", err)
		return err
	}
}
