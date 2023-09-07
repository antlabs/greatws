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

package bigws

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/antlabs/wsutil/fixedwriter"
	"github.com/antlabs/wsutil/frame"
	"github.com/antlabs/wsutil/opcode"
	"golang.org/x/sys/unix"
)

type conn struct {
	fd   int    // 文件描述符fd
	wbuf []byte // 缓冲区, 当直接Write失败时，会将数据写入缓冲区
}

type Conn struct {
	c conn

	mu      sync.Mutex
	client  bool  // 客户端为true，服务端为false
	*Config       // 配置
	closed  int32 // 是否关闭
}

func (c *conn) Write(b []byte) (n int, err error) {
	// 如果缓冲区有数据，将数据写入缓冲区
	curN := len(b)
	if len(c.wbuf) > 0 {
		c.wbuf = append(c.wbuf, b...)
		b = c.wbuf
	}

	// 直接写入数据
	n, err = unix.Write(c.fd, b)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			newBuf := make([]byte, len(b)-n)
			copy(newBuf, b[n:])
			c.wbuf = newBuf
			return curN, nil
		}
	}
	// 出错
	return n, err
}

func (c *Conn) getFd() int {
	return c.c.fd
}

// 该函数有3个动作
// 写成功
// EAGAIN，等待可写再写
// 报错，直接关闭这个fd
func (c *Conn) flushOrClose() {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, err := unix.Write(c.c.fd, c.c.wbuf)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			wbuf := c.c.wbuf
			copy(wbuf, wbuf[n:])
			c.c.wbuf = wbuf[:len(wbuf)-n]
			return
		}
		unix.Close(c.c.fd)
		atomic.StoreInt32(&c.closed, 1)
	}
}

type wrapBuffer struct {
	bytes.Buffer
}

func (w *wrapBuffer) Close() error {
	return nil
}

func (c *Conn) WriteMessage(op Opcode, writeBuf []byte) (err error) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrClosed
	}

	if op == opcode.Text {
		if !c.utf8Check(writeBuf) {
			return ErrTextNotUTF8
		}
	}

	rsv1 := c.compression && (op == opcode.Text || op == opcode.Binary)
	if rsv1 {
		var out wrapBuffer
		w := compressNoContextTakeover(&out, defaultCompressionLevel)
		if _, err = io.Copy(w, bytes.NewReader(writeBuf)); err != nil {
			return
		}

		if err = w.Close(); err != nil {
			return
		}
		writeBuf = out.Bytes()
	}

	// f.Opcode = op
	// f.PayloadLen = int64(len(writeBuf))
	maskValue := uint32(0)
	if c.client {
		maskValue = rand.Uint32()
	}

	var fw fixedwriter.FixedWriter
	c.mu.Lock()
	err = frame.WriteFrame(&fw, &c.c, writeBuf, true, rsv1, c.client, op, maskValue)
	c.mu.Unlock()
	return err
}
