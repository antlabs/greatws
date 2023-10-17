//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

package bigws

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"sync"
	"sync/atomic"
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
	rbuf := make([]byte, 1024+15)
	c := &Conn{
		conn: conn{
			fd:   fd,
			rbuf: &rbuf,
		},
		//wbuf:   make([]byte, 0, 1024),
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
	_, err = c.writeOrAddPoll(b)
	if err != nil {
		return 0, err
	}
	// 出错
	return curN, err
}

func (c *Conn) writeOrAddPoll(b []byte) (n int, err error) {
	total := 0
	for len(b) > 0 {

		// 直接写入数据
		n, err = unix.Write(c.fd, b)
		//fmt.Printf("1.write %d:%v: %d\n", n, err, len(b))

		if err != nil {
			// 如果是EAGAIN或EINTR错误，说明是写缓冲区满了，或者被信号中断，将数据写入缓冲区
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				if n < 0 {
					n = 0
				}
				if len(b) > 0 {
					// TODO
					newBuf := make([]byte, len(b)-n)
					copy(newBuf, b[n:])

					c.wbuf = newBuf

				}
				if err = c.multiEventLoop.addWrite(c); err != nil {
					return 0, err
				}
				return total, nil
			}
			c.multiEventLoop.del(c)

			atomic.StoreInt32(&c.closed, 1)
			return
		}
		if n > 0 {
			b = b[n:]
			total += n
		}
	}

	if len(c.wbuf) == total {
		c.wbuf = nil
	}
	return total, nil
}

// 该函数有3个动作
// 写成功
// EAGAIN，等待可写再写
// 报错，直接关闭这个fd
func (c *Conn) flushOrClose() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.writeOrAddPoll(c.wbuf)
}

// 该函数从缓冲区读取数据，并且解析出websocket frame
// 有几种情况需要处理下
// 1. 缓冲区空间不句够，需要扩容
// 2. 缓冲区数据不够，并且一次性读取了多个frame
func (c *Conn) processWebsocketFrame() (n int, err error) {
	// 1. 处理frame header
	if !c.useIoUring {
		// 不使用io_uring的直接调用read获取buffer数据
		for i := 0; ; i++ {
			n, err = unix.Read(c.fd, (*c.rbuf)[c.rw:])
			//fmt.Printf("i = %d, n = %d, fd = %d, rbuf = %d, rw:%d, err = %v, %v, payload:%d\n", i, n, c.fd, len((*c.rbuf)[c.rw:]), c.rw+n, err, time.Now(), c.rh.PayloadLen)
			if err != nil {
				// 信号中断，继续读
				if errors.Is(err, unix.EINTR) {
					continue
				}
				// 出错返回
				if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
					return 0, err
				}
				// 缓冲区没有数据，等待可读
				err = nil
				break
			}

			if n == 0 && len((*c.rbuf)[c.rw:]) > 0 {
				c.multiEventLoop.del(c)
				c.OnClose(c, io.EOF)
				return
			}

			if n > 0 {
				c.rw += n
			}

			if len((*c.rbuf)[c.rw:]) == 0 {
				// 说明缓存区已经满了。需要扩容
				// 并且如果使用epoll ET mode，需要继续读取，直到返回EAGAIN, 不然会丢失数据
				// 结合以上两种，缓存区满了就直接处理frame，解析出payload的长度，得到一个刚刚好的缓存区
				// for i := 0; len((*c.rbuf)[c.rw:]) == 0 && i < 3; i++ {
				if _, err := c.readHeader(); err != nil {
					return 0, fmt.Errorf("read header err: %w", err)
				}
				if _, err := c.readPayloadAndCallback(); err != nil {
					return 0, fmt.Errorf("read header err: %w", err)
				}

				if len((*c.rbuf)[c.rw:]) == 0 {
					panic(fmt.Sprintf("需要扩容:rw(%d):rr(%d):currState(%v)", c.rw, c.rr, c.curState.String()))
				}
				continue
			}

		}
	}

	for i := 0; ; i++ {
		sucess, err := c.readHeader()
		if err != nil {
			return 0, fmt.Errorf("read header err: %w", err)
		}

		if !sucess {
			return 0, nil
		}
		sucess, err = c.readPayloadAndCallback()
		if err != nil {
			return 0, fmt.Errorf("read header err: %w", err)
		}

		if !sucess {
			return 0, nil
		}
	}
}

func closeFd(fd int) {
	unix.Close(fd)
}
