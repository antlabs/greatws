//go:build linux
// +build linux

package bigws

import (
	"fmt"
	"io"

	"github.com/pawelgaczynski/giouring"
)

func (c *Conn) processRead(cqe *giouring.CompletionQueueEvent) error {
	// 返回值小于等于0，表示读取完毕，关闭连接
	if cqe.Res <= 0 {
		go c.closeAndWaitOnMessage(true, io.EOF)
		c.getLogger().Debug("read res <= 0", "res", cqe.Res, "fd", c.fd)
		return nil
	}

	c.getLogger().Debug("read res", "res", cqe.Res, "fd", c.fd)

	// 处理websocket数据
	c.rw += int(cqe.Res)
	_, err := c.processWebsocketFrameOnlyIoUring()
	if err != nil {
		c.getLogger().Error("processWebsocketFrameOnlyIoUring", "err", err)
		return err
	}

	parent := c.getParent()
	if parent == nil {
		c.processClose(cqe)
		c.getLogger().Info("parent is nil", "close", c.closed)
		return nil
	}

	if err := parent.addRead(c); err != nil {
		return err
	}

	return nil
}

func (c *Conn) processWrite(cqe *giouring.CompletionQueueEvent, writeSeq uint32) error {
	if c.isClosed() {
		return nil
	}

	c.getLogger().Debug("write res", "res", cqe.Res)

	if cqe.Res < 0 {
		c.processClose(cqe)
		c.getLogger().Error("write res < 0", "res", cqe.Res, "fd", c.fd)
		return nil
	}

	v, ok := c.m.Load(writeSeq)
	if !ok {
		return fmt.Errorf("processWrite: fail: writeSeq not found:%d, userData:%x", writeSeq, cqe.UserData)
	}

	ioState, ok := v.(*ioUringWrite)
	if !ok {
		panic("processWrite: fail: freeBuf not found")
	}

	if len(ioState.writeBuf) != int(cqe.Res) {
		panic("processWrite: ioState.writeBuf != res")
	}

	c.m.Delete(writeSeq)
	c.getLogger().Debug("processWrite.Delete", "writeSeq", writeSeq, "res", cqe.Res, "fd", c.fd)
	// 写成功就把free还到池里面
	ioState.free()
	return nil
}

func (c *Conn) processClose(cqe *giouring.CompletionQueueEvent) error {
	go c.closeAndWaitOnMessage(true, io.EOF)
	return nil
}
