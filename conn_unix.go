//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

package bigws

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

type Conn struct {
	conn

	wbuf    []byte // 写缓冲区, 当直接Write失败时，会将数据写入缓冲区
	w       io.Writer
	mu      sync.Mutex
	client  bool  // 客户端为true，服务端为false
	*Config       // 配置
	closed  int32 // 是否关闭
}

func newConn(fd int, client bool, conf *Config) *Conn {
	c := &Conn{
		conn: conn{
			fd:   fd,
			rbuf: make([]byte, 1024),
		},
		wbuf:   make([]byte, 0, 1024),
		Config: conf,
		client: client,
	}
	return c
}

func duplicateSocket(socketFD int) (int, error) {
	return unix.Dup(socketFD)
}

func (c *Conn) Close() {
	c.multiEventLoop.del(c)
	unix.Close(c.fd)
}

func (c *Conn) Write(b []byte) (n int, err error) {
	// c.w 里放的iouring
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
			if n > 0 {
				newBuf := make([]byte, len(b)-n)
				copy(newBuf, b[n:])
				c.wbuf = newBuf
			}
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
			fmt.Printf("%p, read %d bytes, %v, %d, rbuf.len:%d, r:%d, w:%d, %s\n",
				c, n, err, len(c.rbuf[c.rw:]), len(c.rbuf), c.rr, c.rw, c.curState)

			if err != nil {
				// 信号中断，继续读
				if errors.Is(err, unix.EINTR) {
					continue
				}
				// 出错返回
				if !errors.Is(err, unix.EAGAIN) {
					return 0, err
				}
				// 缓冲区没有数据，等待可读
				err = nil
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
			if n > 0 {
				wbuf := c.wbuf
				copy(wbuf, wbuf[n:])
				c.wbuf = wbuf[:len(wbuf)-n]
			}
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
