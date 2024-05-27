// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

package greatws

import "unsafe"

type newConnWrite Conn

func connToNewConn(c *Conn) *newConnWrite {
	return (*newConnWrite)(unsafe.Pointer(c))
}

func (c *newConnWrite) Write(p []byte) (n int, err error) {
	c2 := (*Conn)(unsafe.Pointer(c))
	return connWrite(c2, p)
}
