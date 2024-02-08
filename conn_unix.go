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

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/antlabs/wsutil/bytespool"
	"github.com/antlabs/wsutil/enum"
	"golang.org/x/sys/unix"
)

type writeState int32

const (
	writeDefault writeState = 1 << iota
	writeEagain
	writeSuccess
)

func (s writeState) String() string {
	switch s {
	case writeDefault:
		return "default"
	case writeEagain:
		return "eagain"
	default:
		return "invalid"
	}
}

type Conn struct {
	conn

	wbuf          *[]byte     // 写缓冲区, 当直接Write失败时，会将数据写入缓冲区
	mu            sync.Mutex  // 锁
	*Config                   // 配置
	parent        *EventLoop  // event loop
	currBindGo    *businessGo // 绑定模式下，当前绑定的go程
	streamGo      *taskStream // stream模式下，当前绑定的go程
	closeConnOnce sync.Once   // 关闭一次
	onCloseOnce   sync.Once   // 保证只调用一次OnClose函数
}

func newConn(fd int64, client bool, conf *Config) *Conn {
	c := &Conn{
		conn: conn{
			fd:     fd,
			client: client,
		},
		// 初始化不分配内存，只有在需要的时候才分配
		Config: conf,

		parent: conf.multiEventLoop.getEventLoop(int(fd)),
	}

	if conf.runInGoStrategy == taskStrategyStream {
		c.streamGo = newTaskStream()
	}
	return c
}

// 这是一个空函数，兼容下quickws的接口
func (c *Conn) StartReadLoop() {

}

func duplicateSocket(socketFD int) (int, error) {
	return unix.Dup(socketFD)
}

func (c *Conn) closeInnerWithOnClose(err error, onClose bool) {

	c.closeConnOnce.Do(func() {
		if err != nil {
			err = io.EOF
		}
		switch c.Config.runInGoStrategy {
		case taskStrategyBind:
			if c.getCurrBindGo() != nil {
				c.currBindGo.subBinConnCount()
			}
		case taskStrategyStream:
			c.streamGo.close()

		}
		fd := c.getFd()
		c.getLogger().Debug("close conn", slog.Int64("fd", int64(fd)))
		c.parent.del(c)
		atomic.StoreInt64(&c.fd, -1)
		atomic.AddInt32(&c.closed, 1)
	})

	// 这个必须要放在后面
	if onClose {
		c.onCloseOnce.Do(func() {
			c.OnClose(c, err)
		})
	}

}

func (c *Conn) closeInner(err error) {

	c.closeInnerWithOnClose(err, true)
}

func (c *Conn) closeWithLock(err error) {
	if c.isClosed() {
		return
	}

	c.mu.Lock()
	if c.isClosed() {
		c.mu.Unlock()
		return
	}

	if err == nil {
		err = io.EOF
	}
	c.closeInnerWithOnClose(err, false)

	c.mu.Unlock()

	// 这个必须要放在后面， 不然会死锁，因为Close会调用用户的OnClose
	// 用户的OnClose也有可能调用Close， 所以使用flags来判断是否已经关闭
	c.mu.Lock()
	if atomic.LoadInt32(&c.closed) == 1 {
		c.onCloseOnce.Do(func() {
			c.OnClose(c, err)
		})
	}
	c.mu.Unlock()
}

func (c *Conn) getPtr() int {
	return int(uintptr(unsafe.Pointer(c)))
}

func (c *Conn) Write(b []byte) (n int, err error) {
	if c.isClosed() {
		return 0, ErrClosed
	}
	// 如果缓冲区有数据，合并数据
	curN := len(b)

	if c.wbuf != nil && len(*c.wbuf) > 0 {
		if err = c.flushOrCloseInner(false); err != nil {
			return 0, err
		}

		if c.wbuf != nil && len(*c.wbuf) > 0 {
			old := c.wbuf
			*c.wbuf = append(*c.wbuf, b...)
			c.getLogger().Debug("write message", "wbuf_size", len(*c.wbuf), "newbuf.size", len(b))
			if old != c.wbuf {
				PutPayloadBytes(old)
			}

			return curN, nil
		}
	}

	total, ws, err := c.maybeWriteAll(b)
	if err != nil {
		return 0, err
	}
	old := b
	if total != len(b) {
		// 记录写入的数据，如果有写入，分配一个新的缓冲区
		if total > 0 && ws == writeEagain {
			newBuf := GetPayloadBytes(len(old) - total)
			copy(*newBuf, (old)[total:])
			if c.wbuf != nil {
				PutPayloadBytes(c.wbuf)
			}
			c.wbuf = newBuf
		}

		if err = c.parent.addWrite(c); err != nil {
			return total, err
		}
	}

	// 出错
	return curN, err
}

func (c *Conn) maybeWriteAll(b []byte) (total int, ws writeState, err error) {
	// i 的目的是debug的时候使用
	var n int
	fd := c.getFd()
	for i := 0; len(b) > 0; i++ {

		// 直接写入数据
		n, err = unix.Write(int(fd), b)
		// 统计调用	unix.Write的次数
		c.multiEventLoop.addWriteSyscall()

		if err != nil {
			// 如果是EAGAIN或EINTR错误，说明是写缓冲区满了，或者被信号中断，将数据写入缓冲区
			if errors.Is(err, unix.EINTR) {
				continue
			}

			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
				if n < 0 {
					n = 0
				}

				return total + n, writeEagain, nil
			}

			c.getLogger().Error("writeOrAddPoll", "err", err.Error(), slog.Int64("fd", c.fd), slog.Int("b.len", len(b)))
			c.closeInner(err)
			return total, writeDefault, err
		}

		if n > 0 {
			b = b[n:]
			total += n
		}
	}

	return total, writeSuccess, nil
}

// 该函数有3个动作
// 写成功
// EAGAIN，等待可写再写
// 报错，直接关闭这个fd
func (c *Conn) flushOrCloseInner(needLock bool) (err error) {
	if needLock {
		c.mu.Lock()
		defer c.mu.Unlock()
	}

	if c.isClosed() {
		return ErrClosed
	}

	if c.wbuf != nil {
		old := c.wbuf
		total, ws, err := c.maybeWriteAll(*old)

		if total == len(*old) {

			PutPayloadBytes(old)
			c.wbuf = nil
			if err := c.parent.delWrite(c); err != nil {
				return err
			}
		} else {

			// 记录写入的数据，如果有写入，分配一个新的缓冲区
			if total > 0 && ws == writeEagain {
				newBuf := GetPayloadBytes(len(*old) - total)
				if len(*newBuf) < len(*old)-total {
					panic("newBuf is too small")
				}
				copy(*newBuf, (*old)[total:])
				if c.wbuf != nil {
					PutPayloadBytes(c.wbuf)
				}
				c.wbuf = newBuf
			}

			if err = c.parent.addWrite(c); err != nil {
				return err
			}
		}
		c.getLogger().Debug("flush or close after",
			"total", total,
			"err-is-nil", err == nil,
			"need-write", len(*old),
			"addr", c.getPtr(),
			"closed", c.isClosed(),
			"fd", c.getFd(),
			"write_state", ws.String())
	} else {
		c.getLogger().Debug("wbuf is nil", "fd", c.getFd())
		if err := c.parent.delWrite(c); err != nil {
			return err
		}
	}
	return err
}

func (c *Conn) flushOrClose() (err error) {
	return c.flushOrCloseInner(true)
}

// kqueu/epoll模式下，读取数据
// 该函数从缓冲区读取数据，并且解析出websocket frame
// 有几种情况需要处理下
// 1. 缓冲区空间不句够，需要扩容
// 2. 缓冲区数据不够，并且一次性读取了多个frame
func (c *Conn) processWebsocketFrame() (err error) {
	// 1. 处理frame header
	// if !c.useIoUring() {
	if c.rbuf == nil {
		c.rbuf = bytespool.GetBytes(int(float32(c.rh.PayloadLen+enum.MaxFrameHeaderSize) * c.windowsMultipleTimesPayloadSize))
	}

	n := 0
	var success bool
	// 不使用io_uring的直接调用read获取buffer数据
	for i := 0; ; i++ {
		fd := atomic.LoadInt64(&c.fd)
		n, err = unix.Read(int(fd), (*c.rbuf)[c.rw:])
		c.multiEventLoop.addReadSyscall()
		// fmt.Printf("i = %d, n = %d, fd = %d, rbuf = %d, rw:%d, err = %v, %v, payload:%d\n",
		// i, n, c.fd, len((*c.rbuf)[c.rw:]), c.rw+n, err, time.Now(), c.rh.PayloadLen)
		if err != nil {
			// 信号中断，继续读
			if errors.Is(err, unix.EINTR) {
				continue
			}
			// 出错返回
			if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
				goto fail
			}
			// 缓冲区没有数据，等待可读
			err = nil
			break
		}

		// 读到eof，直接关闭
		if n == 0 && len((*c.rbuf)[c.rw:]) > 0 {
			c.closeWithLock(io.EOF)
			c.onCloseOnce.Do(func() {
				c.OnClose(c, io.EOF)
			})
			err = io.EOF
			goto fail
		}

		if n > 0 {
			c.rw += n
		}

		if len((*c.rbuf)[c.rw:]) == 0 {
			// 说明缓存区已经满了。需要扩容
			// 并且如果使用epoll ET mode，需要继续读取，直到返回EAGAIN, 不然会丢失数据
			// 结合以上两种，缓存区满了就直接处理frame，解析出payload的长度，得到一个刚刚好的缓存区
			if _, err = c.readHeader(); err != nil {
				err = fmt.Errorf("read header err: %w", err)
				goto fail
			}
			if _, err = c.readPayloadAndCallback(); err != nil {
				err = fmt.Errorf("read header err: %w", err)
				goto fail
			}

			// TODO
			if len((*c.rbuf)[c.rw:]) == 0 {
				//
				// panic(fmt.Sprintf("需要扩容:rw(%d):rr(%d):currState(%v)", c.rw, c.rr, c.curState.String()))
			}
			continue
		}
	}

	for i := 0; ; i++ {
		success, err = c.readHeader()
		if err != nil {
			err = fmt.Errorf("read header err: %w", err)
			goto fail
		}

		if !success {
			goto success
		}
		success, err = c.readPayloadAndCallback()
		if err != nil {
			err = fmt.Errorf("read payload err: %w", err)
			goto fail
		}

		if !success {
			goto success
		}
	}

success:
fail:
	// 回收read buffer至内存池中
	if err != nil || c.rbuf != nil && c.rr == c.rw {
		c.rr, c.rw = 0, 0
		bytespool.PutBytes(c.rbuf)
		c.rbuf = nil
	}
	return err
}

func closeFd(fd int) {
	unix.Close(int(fd))
}
