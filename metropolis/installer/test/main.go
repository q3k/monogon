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

// This package runs the installer image in a VM provided with an empty block
// device. It then examines the installer console output and the blok device to
// determine whether the installation process completed without issue.
package installer

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/partition/gpt"

	mctl "source.monogon.dev/metropolis/cli/metroctl/core"
	"source.monogon.dev/metropolis/cli/pkg/datafile"
	"source.monogon.dev/metropolis/node/build/mkimage/osimage"
	"source.monogon.dev/metropolis/pkg/cmd"
	"source.monogon.dev/metropolis/proto/api"
)

// Each variable in this block points to either a test dependency or a side
// effect. These variables are initialized in TestMain using Bazel.
var (
	// installerImage is a filesystem path pointing at the installer image that
	// is generated during the test, and is removed afterwards.
	installerImage string
	// nodeStorage is a filesystem path pointing at the VM block device image
	// Metropolis is installed to during the test. The file is removed afterwards.
	nodeStorage string
)

// runQemu starts a new QEMU process, expecting the given output to appear
// in any line printed. It returns true, if the expected string was found,
// and false otherwise.
//
// QEMU is killed shortly after the string is found, or when the context is
// cancelled.
func runQemu(ctx context.Context, args []string, expectedOutput string) (bool, error) {
	defaultArgs := []string{
		"-machine", "q35", "-accel", "kvm", "-nographic", "-nodefaults",
		"-m", "512",
		"-smp", "2",
		"-cpu", "host",
		"-drive", "if=pflash,format=raw,readonly,file=external/edk2/OVMF_CODE.fd",
		"-drive", "if=pflash,format=raw,snapshot=on,file=external/edk2/OVMF_VARS.fd",
		"-serial", "stdio",
		"-no-reboot",
	}
	qemuArgs := append(defaultArgs, args...)
	pf := cmd.TerminateIfFound(expectedOutput, nil)
	return cmd.RunCommand(ctx, "external/qemu/qemu-x86_64-softmmu", qemuArgs, pf)
}

// runQemuWithInstaller runs the Metropolis Installer in a qemu, performing the
// same search-through-std{out,err} as runQemu.
func runQemuWithInstaller(ctx context.Context, args []string, expectedOutput string) (bool, error) {
	args = append(args, "-drive", "if=virtio,format=raw,snapshot=on,cache=unsafe,file="+installerImage)
	return runQemu(ctx, args, expectedOutput)
}

// getStorage creates a sparse file, given a size expressed in mebibytes, and
// returns a path to that file. It may return an error.
func getStorage(size int64) (string, error) {
	image, err := os.Create(nodeStorage)
	if err != nil {
		return "", fmt.Errorf("couldn't create the block device image at %q: %w", nodeStorage, err)
	}
	if err := syscall.Ftruncate(int(image.Fd()), size*1024*1024); err != nil {
		return "", fmt.Errorf("couldn't resize the block device image at %q: %w", nodeStorage, err)
	}
	image.Close()
	return nodeStorage, nil
}

// qemuDriveParam returns QEMU parameters required to run it with a
// raw-format image at path.
func qemuDriveParam(path string) []string {
	return []string{"-drive", "if=virtio,format=raw,snapshot=off,cache=unsafe,file=" + path}
}

// checkEspContents verifies the presence of the EFI payload inside of image's
// first partition. It returns nil on success.
func checkEspContents(image *disk.Disk) error {
	// Get the ESP.
	fs, err := image.GetFilesystem(1)
	if err != nil {
		return fmt.Errorf("couldn't read the installer ESP: %w", err)
	}
	// Make sure the EFI payload exists by attempting to open it.
	efiPayload, err := fs.OpenFile(osimage.EFIPayloadPath, os.O_RDONLY)
	if err != nil {
		return fmt.Errorf("couldn't open the installer's EFI Payload at %q: %w", osimage.EFIPayloadPath, err)
	}
	efiPayload.Close()
	return nil
}

func TestMain(m *testing.M) {
	installerImage = filepath.Join(os.Getenv("TEST_TMPDIR"), "installer.img")
	nodeStorage = filepath.Join(os.Getenv("TEST_TMPDIR"), "stor.img")

	installer := datafile.MustGet("metropolis/installer/test/kernel.efi")
	bundle := datafile.MustGet("metropolis/installer/test/testos/testos_bundle.zip")
	iargs := mctl.MakeInstallerImageArgs{
		Installer:     bytes.NewBuffer(installer),
		InstallerSize: uint64(len(installer)),
		TargetPath:    installerImage,
		NodeParams:    &api.NodeParameters{},
		Bundle:        bytes.NewBuffer(bundle),
		BundleSize:    uint64(len(bundle)),
	}
	if err := mctl.MakeInstallerImage(iargs); err != nil {
		log.Fatalf("Couldn't create the installer image at %q: %v", installerImage, err)
	}
	// With common dependencies set up, run the tests.
	code := m.Run()
	// Clean up.
	os.Remove(installerImage)
	os.Exit(code)
}

func TestInstallerImage(t *testing.T) {
	// This test examines the installer image, making sure that the GPT and the
	// ESP contents are in order.
	image, err := diskfs.OpenWithMode(installerImage, diskfs.ReadOnly)
	if err != nil {
		t.Errorf("Couldn't open the installer image at %q: %s", installerImage, err.Error())
	}
	// Verify that GPT exists.
	ti, err := image.GetPartitionTable()
	if ti.Type() != "gpt" {
		t.Error("Couldn't verify that the installer image contains a GPT.")
	}
	// Check that the first partition is likely to be a valid ESP.
	pi := ti.GetPartitions()
	esp := (pi[0]).(*gpt.Partition)
	if esp.Start == 0 || esp.End == 0 {
		t.Error("The installer's ESP GPT entry looks off.")
	}
	// Verify that the image contains only one partition.
	second := (pi[1]).(*gpt.Partition)
	if second.Name != "" || second.Start != 0 || second.End != 0 {
		t.Error("It appears the installer image contains more than one partition.")
	}
	// Verify the ESP contents.
	if err := checkEspContents(image); err != nil {
		t.Error(err.Error())
	}
}

func TestNoBlockDevices(t *testing.T) {
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	// No block devices are passed to QEMU aside from the install medium. Expect
	// the installer to fail at the device probe stage rather than attempting to
	// use the medium as the target device.
	expectedOutput := "Couldn't find a suitable block device"
	result, err := runQemuWithInstaller(ctx, nil, expectedOutput)
	if err != nil {
		t.Error(err.Error())
	}
	if result != true {
		t.Errorf("QEMU didn't produce the expected output %q", expectedOutput)
	}
}

func TestBlockDeviceTooSmall(t *testing.T) {
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	// Prepare the block device the installer will install to. This time the
	// target device is too small to host a Metropolis installation.
	imagePath, err := getStorage(64)
	defer os.Remove(imagePath)
	if err != nil {
		t.Errorf(err.Error())
	}

	// Run QEMU. Expect the installer to fail with a predefined error string.
	expectedOutput := "Couldn't find a suitable block device"
	result, err := runQemuWithInstaller(ctx, qemuDriveParam(imagePath), expectedOutput)
	if err != nil {
		t.Error(err.Error())
	}
	if result != true {
		t.Errorf("QEMU didn't produce the expected output %q", expectedOutput)
	}
}

func TestInstall(t *testing.T) {
	ctx, ctxC := context.WithCancel(context.Background())
	defer ctxC()

	// Prepare the block device image the installer will install to.
	storagePath, err := getStorage(4096 + 128 + 128 + 1)
	defer os.Remove(storagePath)
	if err != nil {
		t.Errorf(err.Error())
	}

	// Run QEMU. Expect the installer to succeed.
	expectedOutput := "Installation completed"
	result, err := runQemuWithInstaller(ctx, qemuDriveParam(storagePath), expectedOutput)
	if err != nil {
		t.Error(err.Error())
	}
	if result != true {
		t.Errorf("QEMU didn't produce the expected output %q", expectedOutput)
	}

	// Verify the resulting node image. Check whether the node GPT was created.
	storage, err := diskfs.OpenWithMode(storagePath, diskfs.ReadOnly)
	if err != nil {
		t.Errorf("Couldn't open the resulting node image at %q: %s", storagePath, err.Error())
	}
	// Verify that GPT exists.
	ti, err := storage.GetPartitionTable()
	if ti.Type() != "gpt" {
		t.Error("Couldn't verify that the resulting node image contains a GPT.")
	}
	// Check that the first partition is likely to be a valid ESP.
	pi := ti.GetPartitions()
	esp := (pi[0]).(*gpt.Partition)
	if esp.Name != osimage.ESPVolumeLabel || esp.Start == 0 || esp.End == 0 {
		t.Error("The node's ESP GPT entry looks off.")
	}
	// Verify the system partition's GPT entry.
	system := (pi[1]).(*gpt.Partition)
	if system.Name != osimage.SystemVolumeLabel || system.Start == 0 || system.End == 0 {
		t.Error("The node's system partition GPT entry looks off.")
	}
	// Verify the data partition's GPT entry.
	data := (pi[2]).(*gpt.Partition)
	if data.Name != osimage.DataVolumeLabel || data.Start == 0 || data.End == 0 {
		t.Errorf("The node's data partition GPT entry looks off.")
	}
	// Verify that there are no more partitions.
	fourth := (pi[3]).(*gpt.Partition)
	if fourth.Name != "" || fourth.Start != 0 || fourth.End != 0 {
		t.Error("The resulting node image contains more partitions than expected.")
	}
	// Verify the ESP contents.
	if err := checkEspContents(storage); err != nil {
		t.Error(err.Error())
	}
	storage.File.Close()
	// Run QEMU again. Expect TestOS to launch successfully.
	expectedOutput = "_TESTOS_LAUNCH_SUCCESS_"
	result, err = runQemu(ctx, qemuDriveParam(storagePath), expectedOutput)
	if err != nil {
		t.Error(err.Error())
	}
	if result != true {
		t.Errorf("QEMU didn't produce the expected output %q", expectedOutput)
	}
}
