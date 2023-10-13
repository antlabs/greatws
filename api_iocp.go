//go:build windows
// +build windows

// 参考文档
// https://tboox.org/cn/2018/08/16/coroutine-iocp-some-issues/
// https://github.com/microsoft/Windows-classic-samples/blob/main/Samples/Win7Samples/netds/winsock/iocp/server/IocpServer.Cpp
package bigws

import (
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 使用linkname 导出runtime里面的modws2_32变量, 也可以直接使用syscall.NewLazyDLL("ws2_32.dll")创建导出函数
//
//go:linkname modws2_32 syscall.modws2_32
var modws2_32 *syscall.LazyDLL

var (
	procWSADuplicateSocketW = modws2_32.NewProc("WSADuplicateSocketW")
	procWSASocketW          = modws2_32.NewProc("WSASocketW")
)

func WSADuplicateSocket(handle syscall.Handle, pid uint32, pi *syscall.WSAProtocolInfo) (sockerr error) {
	r0, _, _ := syscall.Syscall(procWSADuplicateSocketW.Addr(), 3, uintptr(handle), uintptr(pid), uintptr(unsafe.Pointer(pi)))
	if r0 != 0 {
		sockerr = syscall.Errno(r0)
	}
	return
}

func WSASocket(af int32, stype int32, protocol int32, pi *syscall.WSAProtocolInfo, g uint32, flags uint32) (handle syscall.Handle, err error) {
	r0, _, e1 := syscall.Syscall6(procWSASocketW.Addr(), 6, uintptr(af), uintptr(stype), uintptr(protocol), uintptr(unsafe.Pointer(pi)), uintptr(g), uintptr(flags))
	handle = syscall.Handle(r0)
	if handle == syscall.InvalidHandle {
		if e1 != 0 {
			err = error(e1)
		} else {
			err = syscall.EINVAL
		}
	}
	return
}

type apiState struct {
	iocp windows.Handle
	sync.Once
	sync.Mutex
}

func (e *EventLoop) apiCreate(flag evFlag) (err error) {
	var state apiState
	state.iocp, err = windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 0)
	e.apiState = &state
	return err
}

func (e *EventLoop) apiFree() {
	e.Do(func() {
		windows.CloseHandle(e.iocp)
	})
}

func (e *EventLoop) addRead(c *Conn) error {
	fd := c.getFd()
	e.Lock()
	tmpIocp, err := windows.CreateIoCompletionPort(windows.Handle(uintptr(fd)), e.iocp, uintptr(unsafe.Pointer(c)), 0)
	if err != nil {
		e.Unlock()
		return err
	}
	e.iocp = tmpIocp
	rbuf := newIocpRBuf(clientIoRead, c)
	if c.iocpRBuf == nil {
		c.iocpRBuf = rbuf
	}
	e.Unlock()

	rbuf.wsabuf.Buf = (c.rbuf)[c.rr:][0]
	rbuf.wsabuf.Len = uint32(len(c.rbuf) - c.rr)
	bytesSent := uint32(0)
	dwFlags := uint32(0)
	// TODO: 如果等于0， 需要给个大的buf
	err = windows.WSARecv(windows.Handle(c.fd), &rbuf.wsabuf, 1, &bytesSent, &dwFlags, &rbuf.overlapped, nil)
	if err != nil {
		return err
	}
	return nil
}

func (e *EventLoop) addWrite(fd int) error {
	return nil
}

func (e *EventLoop) apiPoll(tv time.Duration) (retVal int, err error) {
	var dwIoSize uint32
	var overlapped *windows.Overlapped

	var buf *iocpBuf
	err = windows.GetQueuedCompletionStatus(e.iocp, &dwIoSize, nil, &overlapped, 0)
	if err != nil {
		return 0, nil
	}

	buf = (*iocpBuf)((unsafe.Pointer)(overlapped))
	c := buf.parent

	bytesSent := uint32(0)
	dwFlags := uint32(0)
	switch buf.IOOperation {
	case clientIoWrite:
		buf.nSentBytes += int(dwIoSize)
		// 处理写事件
		if buf.nSentBytes < len(buf.wbuf) {
			buf.IOOperation = clientIoWrite
			buf.wsabuf.Buf = &buf.wbuf[buf.nSentBytes:][0]
			buf.wsabuf.Len = uint32(len(buf.wbuf) - buf.nSentBytes)
			err = windows.WSASend(windows.Handle(c.fd), &buf.wsabuf, 1, &bytesSent, dwFlags, &buf.overlapped, nil)
			if err != nil {
				return 0, err
			}
		} else {

			e.Lock()
			rbuf := c.iocpRBuf
			if rbuf == nil {
				rbuf = newIocpRBuf(clientIoRead, c)
			}
			e.Unlock()
			rbuf.wsabuf.Buf = &c.rbuf[c.rr:][0]
			rbuf.wsabuf.Len = uint32(len(c.rbuf) - c.rr)
			// TODO: 如果等于0， 需要给个大的buf
			err = windows.WSARecv(windows.Handle(c.fd), &rbuf.wsabuf, 1, &bytesSent, &dwFlags, &rbuf.overlapped, nil)
			if err != nil {
				return 0, err
			}
			// 注册下读的事件
		}
	case clientIoRead:
		if _, err := c.processWebsocketFrame(int(dwIoSize)); err != nil {
			return 0, err
		}
		rbuf := c.iocpRBuf
		rbuf.wsabuf.Buf = &c.rbuf[c.rr:][0]
		rbuf.wsabuf.Len = uint32(len(c.rbuf) - c.rr)
		// TODO: 如果等于0， 需要给个大的buf
		err = windows.WSARecv(windows.Handle(c.fd), &rbuf.wsabuf, 1, &bytesSent, &dwFlags, &rbuf.overlapped, nil)
		if err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func (e *EventLoop) apiName() string {
	return "CreateIoCompletionPort"
}

func (e *EventLoop) delWrite(fd int) {
}
