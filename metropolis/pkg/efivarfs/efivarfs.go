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

// This package was written with the aim of easing efivarfs integration.
//
// https://www.kernel.org/doc/html/latest/filesystems/efivarfs.html
package efivarfs

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"

	"golang.org/x/text/encoding/unicode"
)

const (
	Path       = "/sys/firmware/efi/efivars"
	GlobalGuid = "8be4df61-93ca-11d2-aa0d-00e098032b8c"
)

// ExtractString returns EFI variable data based on raw variable file contents.
// It returns string-represented data, or an error.
func ExtractString(contents []byte) (string, error) {
	// Fail if total length is shorter than attribute length.
	if len(contents) < 4 {
		return "", fmt.Errorf("contents too short.")
	}
	// efiUnicode defines the Unicode encoding used by UEFI which is UCS-2
	// Little Endian. For BMP characters UTF-16 is equivalent to UCS-2.
	// See the UEFI Spec 2.9, Sections 33.2.6 and 1.8.1.
	efiUnicode := unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM)
	// Skip attributes, see @linux//Documentation/filesystems:efivarfs.rst for format
	efiVarData := contents[4:]
	espUUIDNullTerminated, err := efiUnicode.NewDecoder().Bytes(efiVarData)
	if err != nil {
		// Pass the decoding error unwrapped.
		return "", err
	}
	// Remove the null suffix.
	return string(bytes.TrimSuffix(espUUIDNullTerminated, []byte{0})), nil
}

// ReadLoaderDevicePartUUID reads the ESP UUID from an EFI variable. It
// depends on efivarfs being already mounted. It returns a correct lowercase
// UUID, or an error.
func ReadLoaderDevicePartUUID() (string, error) {
	// Read the EFI variable file containing the ESP UUID.
	espUuidPath := filepath.Join(Path, "LoaderDevicePartUUID-4a67b082-0a4c-41cf-b6c7-440b29bb8c4f")
	efiVar, err := ioutil.ReadFile(espUuidPath)
	if err != nil {
		return "", fmt.Errorf("couldn't read the LoaderDevicePartUUID file at %q: %w", espUuidPath, err)
	}
	contents, err := ExtractString(efiVar)
	if err != nil {
		return "", fmt.Errorf("couldn't decode an EFI variable: %w", err)
	}
	return strings.ToLower(contents), nil
}
