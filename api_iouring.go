//go:build linux
// +build linux

package bigws

import "github.com/pawelgaczynski/giouring"

type iouringState struct {
	ring        *giouring.Ring
	ringEntries int
}

func (e *iouringState) settingDefault() {
	e.ringEntries = 1024
}

func (e *iouringState) apiCreate() (err error) {
	ring, err := giouring.CreateRing(e.ringEntries)
	e.ring = ring
	return nil
}

func (e *iouringState) addRead(fd int) error {
	e.prepareMultishotAccept(l.fd, cb)
	return nil
}

func (e *iouringState) addWrite(fd int) error {
}

func (e *iouringState) del(fd int) error {
}

func (e *iouring) prepare(op operation) {
	sqe := e.ring.GetSQE()
	if sqe == nil { // submit and retry
		e.submit()
		sqe = l.ring.GetSQE()
	}
	if sqe == nil { // still nothing, add to pending
		e.pending = append(e.pending, op)
		return
	}
	op(sqe)
}

func (l *iouring) prepareRecv(fd int, cb completionCallback) {
	l.prepare(func(sqe *giouring.SubmissionQueueEntry) {
		sqe.PrepareRecvMultishot(fd, 0, 0, 0)
		sqe.Flags = giouring.SqeBufferSelect
		sqe.BufIG = buffersGroupID
		l.callbacks.set(sqe, cb)
	})
}
