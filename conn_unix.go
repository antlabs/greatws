//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

package bigws

import (
	"errors"
	"fmt"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

func duplicateSocket(socketFD int) (int, error) {
	return unix.Dup(socketFD)
}

func (c *Conn) Close() {
	unix.Close(c.fd)
}

func (c *Conn) Write(b []byte) (n int, err error) {
	if c.w != nil {
		return c.w.Write(b)
	}

	// 如果缓冲区有数据，合并数据
	curN := len(b)
	if len(c.wbuf) > 0 {
		c.wbuf = append(c.wbuf, b...)
		b = c.wbuf
	}

	// 直接写入数据
	n, err = unix.Write(c.fd, b)
	if err != nil {
		// 如果是EAGAIN或EINTR错误，说明是写缓冲区满了，或者被信号中断，将数据写入缓冲区
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			newBuf := make([]byte, len(b)-n)
			copy(newBuf, b[n:])
			c.wbuf = newBuf
			c.multiEventLoop.addWrite(c)
			return curN, nil
		}
	}
	// 出错
	return n, err
}

func (c *Conn) processWebsocketFrame() (n int, err error) {
	// 1. 处理frame header
	if !c.useIoUring {
		// 不使用io_uring的直接调用read获取buffer数据
		for {
			n, err = unix.Read(c.fd, c.rbuf[c.rw:])
			fmt.Printf("read %d bytes\n", n)
			if err != nil {
				// TODO: 区别是EAGAIN还是其他错误
				break
			}
			if n <= 0 {
				break
			}

			c.rw += n
		}
	}
	if err := c.readHeader(); err != nil {
		fmt.Printf("read header err: %v\n", err)
	}

	// 2. 处理frame payload
	// TODO 这个函数要放到协程里面运行
	c.readPayloadAndCallback()
	return
}

// 该函数有3个动作
// 写成功
// EAGAIN，等待可写再写
// 报错，直接关闭这个fd
func (c *Conn) flushOrClose() {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, err := unix.Write(c.fd, c.wbuf)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			wbuf := c.wbuf
			copy(wbuf, wbuf[n:])
			c.wbuf = wbuf[:len(wbuf)-n]
			return
		}
		unix.Close(c.fd)
		atomic.StoreInt32(&c.closed, 1)
		return
	}

	// 如果写成功就把write事件从事件循环中删除
	c.multiEventLoop.delWrite(c)
}

func closeFd(fd int) {
	unix.Close(fd)
}
