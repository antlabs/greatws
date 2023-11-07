//go:build linux
// +build linux

package bigws

import (
	"unsafe"

	"github.com/pawelgaczynski/giouring"
)

func processConn(cqe *giouring.CompletionQueueEvent) error {
	c := (*Conn)(unsafe.Pointer(uintptr(cqe.UserData)))
	switch c.operation {
	case opRead:
		return c.processRead(cqe)
	case opWrite:
		return c.processWrite(cqe)
	case opClose:
		return c.processClose(cqe)
	default:
		panic("unknown operation")
	}
}

func (c *Conn) processRead(cqe *giouring.CompletionQueueEvent) error {
	if cqe.Res <= 0 {
		go c.closeAndWaitOnMessage(true)
		return nil
	}
	return nil
}

func (c *Conn) processWrite(cqe *giouring.CompletionQueueEvent) error {
	c.getLogger().Debug("write res", "res", cqe.Res)
	return nil
}

func (c *Conn) processClose(cqe *giouring.CompletionQueueEvent) error {
	return nil
}
