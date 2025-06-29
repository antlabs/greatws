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
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/antlabs/pulse/core"
	"github.com/antlabs/task/task/driver"
	"github.com/antlabs/wsutil/bytespool"
	"github.com/antlabs/wsutil/deflate"
	"github.com/antlabs/wsutil/enum"
	"github.com/antlabs/wsutil/myonce"
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

var (
	ErrInvalidDeadline = errors.New("invalid deadline")
	// 读超时
	ErrReadTimeout = errors.New("read timeout")
	// 写超时
	ErrWriteTimeout = errors.New("write timeout")
)

// Conn大小改变历史，增加上下文接管，从<160到184
type Conn struct {
	conn

	Callback                                    // callback移至conn中
	pd       deflate.PermessageDeflateConf      // 上下文接管的控制参数, 由于每个comm的配置都可能不一样，所以需要放在Conn里面
	mu       sync.Mutex                         // 锁
	*Config                                     // 配置
	deCtx    *deflate.DeCompressContextTakeover // 解压缩上下文
	enCtx    *deflate.CompressContextTakeover   // 压缩上下文
	parent   *EventLoop                         // event loop
	task     driver.TaskExecutor                // 任务，该任务会进协程池里面执行
	rtime    *time.Timer                        // 控制读超时
	wtime    *time.Timer                        // 控制写超时

	// mu2由 onCloseOnce使用, 这里使用新锁只是为了简化维护的难度
	// 也可以共用mu，区别 优点:节约内存，缺点:容易出现死锁和需要精心调试代码
	// 这里选择维护简单
	mu2         sync.Mutex
	onCloseOnce myonce.MyOnce // 保证只调用一次OnClose函数
	closed      int32         // 是否关闭
}

func newConn(fd int64, client bool, conf *Config) (*Conn, error) {
	c := &Conn{
		conn: conn{
			fd:     fd,
			client: client,
		},
		// 初始化不分配内存，只有在需要的时候才分配
		Config: conf,
		parent: conf.multiEventLoop.getEventLoop(int(fd)),
	}

	c.task = c.parent.localTask.newTask(conf.runInGoTask)
	if conf.readTimeout > 0 {
		err := c.setReadDeadline(time.Now().Add(conf.readTimeout))
		if err != nil {
			return nil, err
		}
	}
	return c, nil
}

// 这是一个空函数，兼容下quickws的接口
func (c *Conn) StartReadLoop() {

}

// 这是一个空函数，兼容下quickws的接口
func (c *Conn) ReadLoop() error {
	return nil
}

func duplicateSocket(socketFD int) (int, error) {
	return unix.Dup(socketFD)
}

// 没有加锁的版本，有外层已经有锁保护，所以不需要加锁
func (c *Conn) closeWithoutLockOnClose(err error, onClose bool) {

	if c.isClosed() {
		return
	}

	if err != nil {
		err = io.EOF
	}
	fd := c.getFd()
	c.getLogger().Debug("close conn", slog.Int64("fd", int64(fd)))
	c.parent.del(c)
	atomic.StoreInt64(&c.fd, -1)
	atomic.StoreInt32(&c.closed, 1)

	// 这个必须要放在后面
	if onClose {
		c.onCloseOnce.Do(&c.mu2, func() {
			c.OnClose(c, err)
		})
		if c.task != nil {
			c.task.Close(nil)
		}
	}

}

func (c *Conn) closeNoLock(err error) {

	c.closeWithoutLockOnClose(err, true)
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
	c.closeWithoutLockOnClose(err, false)

	c.mu.Unlock()

	// 这个必须要放在后面， 不然会死锁，因为Close会调用用户的OnClose
	// 用户的OnClose也有可能调用Close， 所以使用flags来判断是否已经关闭
	if atomic.LoadInt32(&c.closed) == 1 {
		c.onCloseOnce.Do(&c.mu2, func() {
			c.OnClose(c, err)
		})
	}
}

func (c *Conn) getPtr() int {
	return int(uintptr(unsafe.Pointer(c)))
}

// Conn Write入口， 原始定义是func (c *Conn) Write() (n int, err error)
// 会引起误用，所以隐藏起来, 作为一个websocket库，直接暴露tcp的write接口也不合适
func connWrite(c *Conn, b []byte) (n int, err error) {
	if c.isClosed() {
		return 0, ErrClosed
	}

	return c.write(b)
}

func (c *Conn) needFlush() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.wbufList) > 0
}

func (c *Conn) flush() {
	if _, err := connWrite(c, nil); err != nil {
		slog.Error("failed to flush write buffer", "error", err)
	}
}

// writeToSocket 尝试将数据写入 socket，并处理中断与临时错误
func (c *Conn) writeToSocket(data []byte) (int, error) {

	n, err := core.Write(c.getFd(), data)
	if err == nil {
		return n, nil
	}
	if err == syscall.EINTR {
		return 0, err // 被信号中断，直接返回
	}
	if err == syscall.EAGAIN {
		return 0, err // 资源暂时不可用
	}
	return 0, err // 其他错误直接返回

}

// appendToWbufList 将数据添加到写缓冲区列表
// 先检查最后一个缓冲区是否有足够空间，如果有就直接append
// 如果没有，将部分数据append到最后一个缓冲区，剩余部分创建新的readBufferSize大小的缓冲区
func (c *Conn) appendToWbufList(data []byte, oldLen int) {
	if len(data) == 0 {
		return
	}

	// 如果wbufList为空，直接创建新的缓冲区
	if len(c.wbufList) == 0 {
		// 使用原始长度，对齐，提升复用率
		newBuf := bytespool.GetBytes(len(data) + oldLen)
		copy(*newBuf, data)
		*newBuf = (*newBuf)[:len(data)]
		c.wbufList = append(c.wbufList, newBuf)
		return
	}

	// 获取最后一个缓冲区
	lastBuf := c.wbufList[len(c.wbufList)-1]
	remainingSpace := cap(*lastBuf) - len(*lastBuf)

	// 如果最后一个缓冲区有足够空间，直接append
	if remainingSpace >= len(data) {
		*lastBuf = append(*lastBuf, data...)
		return
	}

	// 如果空间不够，先填满最后一个缓冲区
	if remainingSpace > 0 {
		*lastBuf = append(*lastBuf, data[:remainingSpace]...)
		data = data[remainingSpace:] // 剩余的数据
	}

	// 为剩余数据创建新的缓冲区（使用readBufferSize大小）
	for len(data) > 0 {
		newBuf := bytespool.GetBytes(len(data) + oldLen)
		copySize := len(data)
		if copySize > cap(*newBuf) {
			copySize = cap(*newBuf)
		}
		copy(*newBuf, data[:copySize])
		*newBuf = (*newBuf)[:copySize]
		c.wbufList = append(c.wbufList, newBuf)
		data = data[copySize:]
	}
}

// handlePartialWrite 处理部分写入的情况，创建新缓冲区存储剩余数据
func (c *Conn) handlePartialWrite(data *[]byte, n int, needAppend bool) error {
	if n < 0 {
		n = 0
	}

	// 如果已经全部写入，不需要创建新缓冲区
	if n >= len(*data) {
		return nil
	}

	remainingData := (*data)[n:]
	if needAppend {
		c.appendToWbufList(remainingData, len(*data))
	} else {
		copy(*data, (*data)[n:])
		*data = (*data)[:len(*data)-n]
	}

	// 部分写入成功，或者全部失败
	// 如果启用了流量背压机制且有部分写入，先删除读事件
	if c.Config.flowBackPressureRemoveRead {
		if delErr := c.eventLoop().delRead(c); delErr != nil {
			slog.Error("failed to delete read event", "error", delErr)
		}
	} else {
		if err := c.eventLoop().addWrite(c); err != nil {
			slog.Error("failed to add write event", "error", err)
			return err
		}
	}

	return nil
}

func (c *Conn) write(data []byte) (int, error) {

	if atomic.LoadInt64(&c.fd) == -1 {
		return 0, net.ErrClosed
	}

	if len(data) == 0 && len(c.wbufList) == 0 {
		return 0, nil
	}

	if len(c.wbufList) == 0 {
		n, err := c.writeToSocket(data)
		if errors.Is(err, core.EAGAIN) || errors.Is(err, core.EINTR) || err == nil {
			if n == len(data) {
				return n, nil
			}
			// 把剩余数据放到缓冲区
			if err := c.handlePartialWrite(&data, n, true); err != nil {
				c.closeNoLock(err)
				return 0, err
			}
			return len(data), nil
		}

		// 发生严重错误
		c.closeNoLock(err)
		return n, err
	}

	if len(data) > 0 {
		c.appendToWbufList(data, len(data))
	}

	i := 0
	for i < len(c.wbufList) {
		wbuf := c.wbufList[i]
		n, err := c.writeToSocket(*wbuf)
		if errors.Is(err, core.EAGAIN) || errors.Is(err, core.EINTR) || err == nil /*写入成功，也有n != len(*wbuf)的情况*/ {
			if n == len(*wbuf) {
				bytespool.PutBytes(wbuf)
				c.wbufList[i] = nil
				i++
				continue
			}
			// 移动剩余数据到缓冲区开始位置
			if err := c.handlePartialWrite(wbuf, n, false); err != nil {
				c.closeNoLock(err)
				return 0, err
			}

			// 移动未处理的缓冲区到列表开始位置
			copy(c.wbufList, c.wbufList[i:])
			c.wbufList = c.wbufList[:len(c.wbufList)-i]
			return len(data), nil
		}

		c.closeNoLock(err)
		return n, err
	}

	// 所有数据都已写入
	c.wbufList = c.wbufList[:0]
	// 需要进的逻辑
	// 1.如果是垂直触发模式，并且启用了流量背压机制，重新添加读事件
	// 2.如果是水平触发模式也重新添加读事件，为了去掉写事件
	// 3.如果是垂直触发模式，并且启用了流量背压机制，则需要添加读事件

	// 不需要进的逻辑
	// 1.如果是垂直触发模式，并且没有启用流量背压机制，不需要重新添加事件, TODO

	if err := c.eventLoop().ResetRead(c.getFd()); err != nil {
		slog.Error("failed to reset read event", "error", err)
	}
	return len(data), nil
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
		c.rbuf = bytespool.GetBytes(int(float32(c.rh.PayloadLen)*c.windowsMultipleTimesPayloadSize) + enum.MaxFrameHeaderSize)
	}

	if c.readTimeout > 0 {
		// if err = c.setReadDeadline(time.Time{}); err != nil {
		// 	return err
		// }
		c.setReadDeadline(time.Now().Add(c.readTimeout))
	}
	n := 0
	var success bool
	// 不使用io_uring的直接调用read获取buffer数据
	for i := 0; ; i++ {
		fd := atomic.LoadInt64(&c.fd)
		c.mu.Lock()
		n, err = unix.Read(int(fd), (*c.rbuf)[c.rw:])
		c.mu.Unlock()
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
			c.onCloseOnce.Do(&c.mu2, func() {
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
			// if len((*c.rbuf)[c.rw:]) == 0 {
			// 	//
			// 	// panic(fmt.Sprintf("需要扩容:rw(%d):rr(%d):currState(%v)", c.rw, c.rr, c.curState.String()))
			// }
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

	if err != nil {
		// 如果是status code类型，要回写符合rfc的close包
		c.writeAndMaybeOnClose(err)
	}
	return err
}

func (c *Conn) processHeaderPayloadCallback() (err error) {
	var success bool
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

func (c *Conn) setDeadlineInner(t **time.Timer, tm time.Time, err error) error {
	if t == nil {
		return nil
	}
	c.mu.Lock()
	// c.getLogger().Error("Conn-lock", "addr", uintptr(unsafe.Pointer(c)))
	defer func() {
		// c.getLogger().Error("Conn-unlock", "addr", uintptr(unsafe.Pointer(c)))
		c.mu.Unlock()
	}()
	if tm.IsZero() {
		if *t != nil {
			// c.getLogger().Error("conn-reset", "addr", uintptr(unsafe.Pointer(c)))
			(*t).Stop()
			*t = nil
		}
		return nil
	}

	d := time.Until(tm)
	if d < 0 {
		return ErrInvalidDeadline
	}

	if *t == nil {
		*t = afterFunc(d, func() {
			c.closeWithLock(err)
		})
	} else {
		(*t).Reset(d)
	}
	return nil
}

func (c *Conn) setReadDeadline(t time.Time) error {
	return c.setDeadlineInner(&c.rtime, t, ErrReadTimeout)
}

func (c *Conn) setWriteDeadline(t time.Time) error {
	return c.setDeadlineInner(&c.wtime, t, ErrWriteTimeout)

}
func closeFd(fd int) {
	unix.Close(int(fd))
}

func (c *Conn) eventLoop() *EventLoop {
	return c.parent
}
