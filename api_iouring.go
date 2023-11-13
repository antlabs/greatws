//go:build linux
// +build linux

package bigws

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"syscall"
	"time"
	"unsafe"

	"github.com/pawelgaczynski/giouring"
)

const (
	batchSize      = 128
	buffersGroupID = 0 // currently using only 1 provided buffer group
)

type iouringState struct {
	ring        *giouring.Ring // ring 对象
	ringEntries uint32
	parent      *EventLoop
}

func apiIoUringCreate(el *EventLoop, ringEntries uint32) (la linuxApi, err error) {
	var iouringState iouringState
	ring, err := giouring.CreateRing(ringEntries)
	iouringState.ring = ring
	iouringState.parent = el
	return &iouringState, nil
}

func (e *iouringState) apiFree() {
}

type iouringConn struct {
	*iouringState
	fd int
}

// iouring 模式下，读取数据
func (c *Conn) processWebsocketFrameOnlyIoUring() (n int, err error) {
	// 尽可能消耗完rbuf里面的数据
	for {
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

func (e *iouringState) addRead(c *Conn) error {
	entry := e.ring.GetSQE()
	if entry == nil {
		return errors.New("addRead: fail:GetSQE is nil")
	}

	entry.PrepareRecv(
		c.fd,
		uintptr((*reflect.SliceHeader)(unsafe.Pointer(c.rbuf)).Data+uintptr(c.rr)),
		uint32(len((*c.rbuf)[c.rr:])),
		0)
	entry.UserData = uint64(uintptr(unsafe.Pointer(c)))
	c.operation |= opRead
	return nil
}

func (e *iouringState) addWrite(c *Conn) error {
	entry := e.ring.GetSQE()
	if entry == nil {
		return errors.New("addRead: fail:GetSQE is nil")
	}

	// debug
	// entry.PrepareSend(
	// 	c.fd,
	// 	uintptr((*reflect.SliceHeader)(unsafe.Pointer(&debug)).Data),
	// 	uint32(len(debug)),
	// 	0)

	// entry.PrepareSend(
	// 	c.fd,
	// 	uintptr(c.outboundBuffer.ReadAddress()),
	// 	uint32(c.outboundBuffer.Buffered()),
	// 	0)
	entry.UserData = uint64(uintptr(unsafe.Pointer(c)))
	return nil
}

func (e *iouringState) del(c *Conn) error {
	fd := c.fd
	entry := e.ring.GetSQE()
	if entry == nil {
		return errors.New("del: fail: GetSQE is nil")
	}

	entry.PrepareClose(fd)
	entry.UserData = uint64(uintptr(unsafe.Pointer(c)))

	return nil
}

func (e *iouringState) getLogger() *slog.Logger {
	return e.parent.parent.Logger
}

func (e *iouringState) advance(n uint32) {
	e.ring.CQAdvance(n)
}

func (e *iouringState) run(timeout time.Duration) error {
	var err error
	cqes := make([]*giouring.CompletionQueueEvent, 256 /*TODO:*/)

	ts := syscall.NsecToTimespec(int64(timeout))

	_, err = e.ring.SubmitAndWaitTimeout(256 /*TODO*/, &ts, nil)
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) ||
		errors.Is(err, syscall.ETIME) {
		return nil
	}
	numberOfCQEs := e.ring.PeekBatchCQE(cqes)

	var i uint32
	for i = 0; i < numberOfCQEs; i++ {
		cqe := cqes[i]

		err = processConn(cqe)
		if err != nil {
			e.advance(i + 1)
			return err
		}
	}
	e.advance(numberOfCQEs)

	return nil
}

func (e *iouringState) apiPoll(tv time.Duration) (retVal int, err error) {
	if err := e.run(time.Millisecond * 333); err != nil {
		return 0, err
	}
	return 0, nil
}

func (e *iouringState) delWrite(c *Conn) error {
	return nil
}

func (e *iouringState) apiName() string {
	return "io_uring"
}
