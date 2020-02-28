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

package storage

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"git.monogon.dev/source/nexantic.git/core/pkg/tpm"
)

const (
	dataMountPath         = "/data"
	espMountPath          = "/esp"
	espDataPath           = espMountPath + "/EFI/smalltown"
	etcdSealedKeyLocation = espDataPath + "/data-key.bin"
)

type Manager struct {
	logger              *zap.Logger
	dataReady           bool
	initializationError error
	mutex               sync.RWMutex
}

func Initialize(logger *zap.Logger) (*Manager, error) {
	if err := FindPartitions(); err != nil {
		return nil, err
	}

	if err := os.Mkdir("/esp", 0755); err != nil {
		return nil, err
	}

	// We're mounting ESP sync for reliability, this lowers our chances of getting half-written files
	if err := unix.Mount(ESPDevicePath, espMountPath, "vfat", unix.MS_NOEXEC|unix.MS_NODEV|unix.MS_SYNC, ""); err != nil {
		return nil, err
	}

	manager := &Manager{
		logger:    logger,
		dataReady: false,
	}

	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	return manager, nil
}

var keySize uint16 = 256 / 8

// MountData mounts the Smalltown data partition with the given global unlock key. It automatically
// unseals the local unlock key from the TPM.
func (s *Manager) MountData(globalUnlockKey []byte) error {
	localPath, err := s.GetPathInPlace(PlaceESP, "local_unlock.bin")
	if err != nil {
		return fmt.Errorf("failed to find ESP mount: %w", err)
	}
	localUnlockBlob, err := ioutil.ReadFile(localPath)
	if err != nil {
		return fmt.Errorf("failed to read local unlock file from ESP: %w", err)
	}
	localUnlockKey, err := tpm.Unseal(localUnlockBlob)
	if err != nil {
		return fmt.Errorf("failed to unseal local unlock key: %w", err)
	}

	key := make([]byte, keySize)
	for i := uint16(0); i < keySize; i++ {
		key[i] = localUnlockKey[i] ^ globalUnlockKey[i]
	}

	if err := MapEncryptedBlockDevice("data", SmalltownDataCryptPath, key); err != nil {
		return err
	}
	if err := s.mountData(); err != nil {
		return err
	}
	s.mutex.Lock()
	s.dataReady = true
	s.mutex.Unlock()
	s.logger.Info("Mounted encrypted storage")
	return nil
}

// InitializeData initializes the Smalltown data partition and returns the global unlock key. It seals
// the local portion into the TPM and stores the blob on the ESP. This is a potentially slow
// operation since it touches the whole partition.
func (s *Manager) InitializeData() ([]byte, error) {
	localUnlockKey, err := tpm.GenerateSafeKey(keySize)
	if err != nil {
		return []byte{}, fmt.Errorf("failed to generate safe key: %w", err)
	}
	globalUnlockKey, err := tpm.GenerateSafeKey(keySize)
	if err != nil {
		return []byte{}, fmt.Errorf("failed to generate safe key: %w", err)
	}

	localUnlockBlob, err := tpm.Seal(localUnlockKey, tpm.SecureBootPCRs)
	if err != nil {
		return []byte{}, fmt.Errorf("failed to seal local unlock key: %w", err)
	}

	// The actual key is generated by XORing together the localUnlockKey and the globalUnlockKey
	// This provides us with a mathematical guarantee that the resulting key cannot be recovered
	// whithout knowledge of both parts.
	key := make([]byte, keySize)
	for i := uint16(0); i < keySize; i++ {
		key[i] = localUnlockKey[i] ^ globalUnlockKey[i]
	}

	if err := InitializeEncryptedBlockDevice("data", SmalltownDataCryptPath, key); err != nil {
		s.logger.Error("Failed to initialize encrypted block device", zap.Error(err))
		return []byte{}, fmt.Errorf("failed to initialize encrypted block device: %w", err)
	}
	mkfsCmd := exec.Command("/bin/mkfs.xfs", "-qf", "/dev/data")
	if _, err := mkfsCmd.Output(); err != nil {
		s.logger.Error("Failed to format encrypted block device", zap.Error(err))
		return []byte{}, fmt.Errorf("failed to format encrypted block device: %w", err)
	}

	if err := s.mountData(); err != nil {
		return []byte{}, err
	}

	s.mutex.Lock()
	s.dataReady = true
	s.mutex.Unlock()

	localPath, err := s.GetPathInPlace(PlaceESP, "local_unlock.bin")
	if err != nil {
		return []byte{}, fmt.Errorf("failed to find ESP mount: %w", err)
	}
	if err := ioutil.WriteFile(localPath, localUnlockBlob, 0600); err != nil {
		return []byte{}, fmt.Errorf("failed to write local unlock file to ESP: %w", err)
	}

	s.logger.Info("Initialized encrypted storage")
	return globalUnlockKey, nil
}

func (s *Manager) mountData() error {
	if err := os.Mkdir("/data", 0755); err != nil {
		return err
	}

	if err := unix.Mount("/dev/data", "/data", "xfs", unix.MS_NOEXEC|unix.MS_NODEV, ""); err != nil {
		return err
	}
	return nil
}

// GetPathInPlace returns a path in the given place
// It may return ErrNotInitialized if the place you're trying to access
// is not initialized or ErrUnknownPlace if the place is not known
func (s *Manager) GetPathInPlace(place DataPlace, path string) (string, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	switch place {
	case PlaceESP:
		return filepath.Join(espDataPath, path), nil
	case PlaceData:
		if s.dataReady {
			return filepath.Join(dataMountPath, path), nil
		}
		return "", ErrNotInitialized
	default:
		return "", ErrUnknownPlace
	}
}
