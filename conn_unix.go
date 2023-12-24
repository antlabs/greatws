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
	"golang.org/x/sys/unix"
)

type ioUringOpState uint32

const (
	connInvalid ioUringOpState = 1 << iota
	opRead
	opWrite
	opClose
)

func (s ioUringOpState) String() string {
	switch s {
	case opRead:
		return "read"
	case opWrite:
		return "write"
	case opClose:
		return "close"
	default:
		return "invalid"
	}
}

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

	// 存在io-uring相关的控制信息
	// onlyIoUringState

	wbuf      *[]byte // 写缓冲区, 当直接Write失败时，会将数据写入缓冲区
	mu        sync.Mutex
	client    bool  // 客户端为true，服务端为false
	*Config         // 配置
	closed    int32 // 是否关闭
	closeOnce sync.Once
	parent    *EventLoop
}

func (c *Conn) setParent(el *EventLoop) {
	atomic.StorePointer((*unsafe.Pointer)((unsafe.Pointer)(&c.parent)), unsafe.Pointer(el))
}

func (c *Conn) getParent() *EventLoop {
	return (*EventLoop)(atomic.LoadPointer((*unsafe.Pointer)((unsafe.Pointer)(&c.parent))))
}

func newConn(fd int64, client bool, conf *Config) *Conn {
	rbuf := bytespool.GetBytes(conf.initPayloadSize())
	c := &Conn{
		conn: conn{
			fd:   fd,
			rbuf: rbuf,
		},
		// 初始化不分配内存，只有在需要的时候才分配
		Config: conf,
		client: client,
	}

	return c
}

func duplicateSocket(socketFD int) (int, error) {
	return unix.Dup(socketFD)
}

func (c *Conn) closeInner(err error) {
	fd := c.getFd()
	c.getLogger().Debug("close conn", slog.Int64("fd", int64(fd)))
	c.multiEventLoop.del(c)
	atomic.StoreInt64(&c.fd, -1)
	c.closeOnce.Do(func() {
		atomic.StorePointer((*unsafe.Pointer)((unsafe.Pointer)(&c.parent)), nil)
	})
	atomic.StoreInt32(&c.closed, 1)
}

func (c *Conn) closeWithLock(err error) {
	if c.isClosed() {
		return
	}

	c.mu.Lock()
	c.closeInner(err)
	c.mu.Unlock()
}

func (c *Conn) Close() {
	c.closeWithLock(nil)
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
			b = *c.wbuf
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

		if err = c.multiEventLoop.addWrite(c); err != nil {
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
			if err := c.multiEventLoop.delWrite(c); err != nil {
				return err
			}
		} else {

			// 记录写入的数据，如果有写入，分配一个新的缓冲区
			if total > 0 && ws == writeEagain {
				newBuf := GetPayloadBytes(len(*old) - total)
				copy(*newBuf, (*old)[total:])
				if c.wbuf != nil {
					PutPayloadBytes(c.wbuf)
				}
				c.wbuf = newBuf
			}

			if err = c.multiEventLoop.addWrite(c); err != nil {
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
		if err := c.multiEventLoop.delWrite(c); err != nil {
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
func (c *Conn) processWebsocketFrame() (n int, err error) {
	// 1. 处理frame header
	if !c.useIoUring() {
		// 不使用io_uring的直接调用read获取buffer数据
		for i := 0; ; i++ {
			fd := atomic.LoadInt64(&c.fd)
			n, err = unix.Read(int(fd), (*c.rbuf)[c.rw:])
			c.multiEventLoop.addReadSyscall()
			// fmt.Printf("i = %d, n = %d, fd = %d, rbuf = %d, rw:%d, err = %v, %v, payload:%d\n", i, n, c.fd, len((*c.rbuf)[c.rw:]), c.rw+n, err, time.Now(), c.rh.PayloadLen)
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

			// 读到eof，直接关闭
			if n == 0 && len((*c.rbuf)[c.rw:]) > 0 {
				c.closeWithLock(io.EOF)
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
				if _, err := c.readHeader(); err != nil {
					return 0, fmt.Errorf("read header err: %w", err)
				}
				if _, err := c.readPayloadAndCallback(); err != nil {
					return 0, fmt.Errorf("read header err: %w", err)
				}

				// TODO
				if len((*c.rbuf)[c.rw:]) == 0 {
					//
					// panic(fmt.Sprintf("需要扩容:rw(%d):rr(%d):currState(%v)", c.rw, c.rr, c.curState.String()))
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
	unix.Close(int(fd))
}
