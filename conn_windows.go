//go:build windows
// +build windows

package bigws

import (
	"fmt"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

type IOOperation uint8

const (
	clientIoRead IOOperation = 1 << iota
	clientIoWrite
)

type Conn struct {
	conn

	mu      sync.Mutex
	client  bool  // 客户端为true，服务端为false
	*Config       // 配置
	closed  int32 // 是否关闭

	iocpRBuf *iocpBuf
}

type iocpBuf struct {
	overlapped  windows.Overlapped
	IOOperation // 操作类型
	parent      *Conn
	wsabuf      windows.WSABuf
	wbuf        []byte
	nSentBytes  int // 写的字节数
}

func newIocpWBuf(op IOOperation, parent *Conn, p []byte) *iocpBuf {
	// TODO sync.Pool
	b := make([]byte, len(p))

	buf := &iocpBuf{
		IOOperation: op,
		parent:      parent,
	}

	buf.wsabuf.Len = uint32(len(b))
	buf.wsabuf.Buf = &b[0]
	return buf
}

func newIocpRBuf(op IOOperation, parent *Conn) *iocpBuf {
	buf := &iocpBuf{
		IOOperation: op,
		parent:      parent,
	}
	buf.wsabuf.Len = uint32(len(parent.rbuf))
	buf.wsabuf.Buf = &parent.rbuf[0]
	return buf
}

func newConn(fd int, client bool, conf *Config) *Conn {
	c := &Conn{
		conn: conn{
			fd:   fd,
			rbuf: make([]byte, 1024),
		},
		Config: conf,
		client: client,
	}
	return c
}

func duplicateSocket(socketFD int) (int, error) {
	var pi syscall.WSAProtocolInfo
	err := WSADuplicateSocket(syscall.Handle(socketFD), uint32(syscall.Getpid()), &pi)
	if err != nil {
		return 0, fmt.Errorf("WSADuplicateSocket:%w", err)
	}

	h, err := WSASocket(-1, -1, -1, &pi, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("WSASocket:%w", err)
	}

	return int(h), nil
}

func (c *Conn) Close() {
	windows.Closesocket(windows.Handle(c.fd))
}

// TODO: 验证下write buffer如果写一半，这时候应用层又WriteMessage了一次，有没有问题
func (c *Conn) Write(b []byte) (n int, err error) {
	buf := newIocpWBuf(clientIoWrite, c, b)
	bytesSent := uint32(0)
	dwFlags := uint32(0)
	err = windows.WSASend(windows.Handle(c.fd), &buf.wsabuf, 1, &bytesSent, dwFlags, &buf.overlapped, nil)
	if err != nil {
		return 0, err
	}

	return len(b), nil
}

func (c *Conn) processWebsocketFrame(w int) (n int, err error) {
	c.rw += n
	if err := c.readHeader(); err != nil {
		fmt.Printf("read header err: %v\n", err)
	}

	// 2. 处理frame payload
	// TODO 这个函数要放到协程里面运行
	c.readPayloadAndCallback()
	return
}

func (c *Conn) flushOrClose() {
}

func closeFd(fd int) {
	windows.Closesocket(windows.Handle(fd))
}
