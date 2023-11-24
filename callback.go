// Copyright 2021-2023 antlabs. All rights reserved.
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
package greatws

type (
	Callback interface {
		OnOpen(*Conn)
		OnMessage(*Conn, Opcode, []byte)
		OnClose(*Conn, error)
	}
)

type (
	OnOpenFunc func(*Conn)
)

// 1. 默认的OnOpen, OnMessage, OnClose都是空函数
type DefCallback struct{}

func (defcallback *DefCallback) OnOpen(_ *Conn) {
}

func (defcallback *DefCallback) OnMessage(_ *Conn, _ Opcode, _ []byte) {
}

func (defcallback *DefCallback) OnClose(_ *Conn, _ error) {
}

// 2. 只设置OnMessage, 和OnClose互斥
type OnMessageFunc func(*Conn, Opcode, []byte)

func (o OnMessageFunc) OnOpen(_ *Conn) {
}

func (o OnMessageFunc) OnMessage(c *Conn, op Opcode, data []byte) {
	o(c, op, data)
}

func (o OnMessageFunc) OnClose(_ *Conn, _ error) {
}

// 3. 只设置OnClose, 和OnMessage互斥
type OnCloseFunc func(*Conn, error)

func (o OnCloseFunc) OnOpen(_ *Conn) {
}

func (o OnCloseFunc) OnMessage(_ *Conn, _ Opcode, _ []byte) {
}

func (o OnCloseFunc) OnClose(c *Conn, err error) {
	o(c, err)
}

// 4. 函数转换为接口
type funcToCallback struct {
	onOpen    func(*Conn)
	onMessage func(*Conn, Opcode, []byte)
	onClose   func(*Conn, error)
}

func (f *funcToCallback) OnOpen(c *Conn) {
	if f.onOpen != nil {
		f.onOpen(c)
	}
}

func (f *funcToCallback) OnMessage(c *Conn, op Opcode, data []byte) {
	if f.onMessage != nil {
		f.onMessage(c, op, data)
	}
}

func (f *funcToCallback) OnClose(c *Conn, err error) {
	if f.onClose != nil {
		f.onClose(c, err)
	}
}

type goCallback struct {
	c Callback
	t *task
}

func newGoCallback(c Callback, t *task) *goCallback {
	return &goCallback{c: c, t: t}
}

func (g *goCallback) OnOpen(c *Conn) {
	g.c.OnOpen(c)
}

func (g *goCallback) OnMessage(c *Conn, op Opcode, data []byte) {
	//	g.c.OnMessage(c, op, data)
	c.waitOnMessageRun.Add(1)
	// need := c.rh.Mask
	// maskKey := c.rh.MaskKey
	g.t.addTask(func() (exit bool) {
		defer c.waitOnMessageRun.Done()

		// if need {
		// 	mask.Mask(data, maskKey)
		// }
		g.c.OnMessage(c, op, data)
		PutPayloadBytes(&data)
		return false
	})
}

func (g *goCallback) OnClose(c *Conn, err error) {
	g.c.OnClose(c, err)
}
