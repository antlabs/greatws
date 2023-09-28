//go:build windows
// +build windows

// 参考文档
// https://tboox.org/cn/2018/08/16/coroutine-iocp-some-issues/
// https://github.com/tboox/tbox/blob/6d19e51563d005660d3789bf218d9e376fc84b06/src/tbox/platform/windows/poller_iocp.c#L53
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
	e.iocp, err = windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 0)
	return err
}

func (e *EventLoop) apiFree() {
	e.Do(func() {
		windows.CloseHandle(e.iocp)
	})
}

func (e *EventLoop) addRead(fd int) error {
	e.Lock()
	tmpIocp, err := windows.CreateIoCompletionPort(windows.Handle(fd), e.iocp, 0, 0)
	if err != nil {
		e.Unlock()
		return err
	}
	e.iocp = tmpIocp
	e.Unlock()
	return nil
}

func (e *EventLoop) addWrite(fd int) error {
	return nil
}

func (e *EventLoop) apiPoll(tv time.Duration) (retVal int, err error) {
	var qty *uint32
	windows.GetQueuedCompletionStatus(e.iocp, qty, &fd, &overlapped, 0)
}

func (e *EventLoop) apiName() string {
	return "CreateIoCompletionPort"
}

func (e *EventLoop) delWrite(fd int) {
}
