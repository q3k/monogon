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

package declarative

import (
	"fmt"
	"io/ioutil"
	"os"
)

// FSRoot is a root of a storage backend that resides on the local filesystem.
type FSRoot struct {
	// The local path at which the declarative directory structure is located (eg. "/").
	root string
}

type FSPlacement struct {
	root *FSRoot
	path string
}

func (f *FSPlacement) FullPath() string {
	return f.path
}

func (f *FSPlacement) RootRef() interface{} {
	return f.root
}

func (f *FSPlacement) Exists() (bool, error) {
	_, err := os.Stat(f.FullPath())
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (f *FSPlacement) Read() ([]byte, error) {
	return ioutil.ReadFile(f.FullPath())
}

func (f *FSPlacement) Write(d []byte, mode os.FileMode) error {
	return ioutil.WriteFile(f.FullPath(), d, mode)
}

// PlaceFS takes a pointer to a Directory or a pointer to a structure embedding Directory and places it at a given
// filesystem root. From this point on the given structure pointer has valid Placement interfaces.
func PlaceFS(dd interface{}, root string) error {
	r := &FSRoot{root}
	pathFor := func(parent, this string) string {
		var np string
		switch {
		case parent == "" && this == "":
			np = "/"
		case parent == "/":
			np = "/" + this
		default:
			np = fmt.Sprintf("%s/%s", parent, this)
		}
		return np
	}
	dp := func(parent, this string) DirectoryPlacement {
		np := pathFor(parent, this)
		return &FSPlacement{path: np, root: r}
	}
	fp := func(parent, this string) FilePlacement {
		np := pathFor(parent, this)
		return &FSPlacement{path: np, root: r}
	}
	err := place(dd, "", "", dp, fp)
	if err != nil {
		return fmt.Errorf("could not place: %w", err)
	}
	return nil
}
