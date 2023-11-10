//go:build linux
// +build linux

package bigws

import (
	"io"
	"unsafe"

	"github.com/pawelgaczynski/giouring"
)

// io-uring 处理事件的入口函数
func processConn(cqe *giouring.CompletionQueueEvent) error {
	c := (*Conn)(unsafe.Pointer(uintptr(cqe.UserData)))
	operation := c.operation
	if operation&opRead > 0 {
		if err := c.processRead(cqe); err != nil {
			return err
		}
	}
	if operation&opWrite > 0 {
		if err := c.processWrite(cqe); err != nil {
			return err
		}
	}
	if operation&opClose > 0 {
		if err := c.processClose(cqe); err != nil {
			return err
		}
	}
	return nil
}

// 读取数据
func (c *Conn) processRead(cqe *giouring.CompletionQueueEvent) error {
	// 返回值小于等于0，表示读取完毕，关闭连接
	if cqe.Res <= 0 {
		go c.closeAndWaitOnMessage(true, io.EOF)
		return nil
	}

	c.getLogger().Debug("read res", "res", cqe.Res, "fd", c.fd)

	// 处理websocket数据
	_, err := c.processWebsocketFrameOnlyIoUring(unsafe.Slice((*byte)(c.inboundBuffer.WriteAddress()), cqe.Res))
	if err != nil {
		c.getLogger().Error("processWebsocketFrameOnlyIoUring", "err", err)
		return err
	}

	// 修改buffer读取位置
	c.inboundBuffer.AdvanceWrite(int(cqe.Res))
	if err := c.multiEventLoop.add(c); err != nil {
		return err
	}

	// 如果outputBuffer有数据，就写入到socket里面
	if c.outboundBuffer.Buffered() > 0 {
		return c.multiEventLoop.addWrite(c)
	}
	return nil
}

func (c *Conn) processWrite(cqe *giouring.CompletionQueueEvent) error {
	c.getLogger().Debug("write res", "res", cqe.Res)
	c.outboundBuffer.AdvanceWrite(int(cqe.Res))
	return nil
}

func (c *Conn) processClose(cqe *giouring.CompletionQueueEvent) error {
	go c.closeAndWaitOnMessage(true, io.EOF)
	return nil
}
