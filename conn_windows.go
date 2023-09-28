//go:build windows
// +build windows

package bigws

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/windows"
)

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

func (c *Conn) Write(b []byte) (n int, err error) {
	return
}

func (c *Conn) processWebsocketFrame() (n int, err error) {
	return
}

func (c *Conn) flushOrClose() {
}

func closeFd(fd int) {
	windows.Closesocket(windows.Handle(fd))
}
