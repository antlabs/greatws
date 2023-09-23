//go:build linux
// +build linux

package bigws

import (
	"context"
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/pawelgaczynski/giouring"
)

type (
	completionCallback = func(res int32, flags uint32, err *ErrErrno)
	iouringState       struct {
		ring        *giouring.Ring
		ringEntries uint32
		parent      *EventLoop
	}
)

func (e *iouringState) settingDefault() {
	e.ringEntries = 1024
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

func (e *iouringState) addRead(fd int) error {
	e.prepareMultishotAccept(fd, cb)
	return nil
}

func (e *iouringState) addWrite(fd int) error {
	return nil
}

func (e *iouringState) del(fd int) error {
	return nil
}

func (e *iouringState) flushCompletions() uint32 {
	var cqes [batchSize]*giouring.CompletionQueueEvent
	var noCompleted uint32 = 0
	for {
		peeked := e.ring.PeekBatchCQE(cqes[:])
		for _, cqe := range cqes[:peeked] {
			err := cqeErr(cqe)
			if cqe.UserData == 0 {
				slog.Debug("ceq without userdata", "res", cqe.Res, "flags", cqe.Flags, "err", err)
				continue
			}
			cb := e.callbacks.get(cqe)
			cb(cqe.Res, cqe.Flags, err)
		}
		e.ring.CQAdvance(peeked)
		noCompleted += peeked
		if peeked < uint32(len(cqes)) {
			return noCompleted
		}
	}
}

func (e *iouringState) runOnce() error {
	if err := e.submitAndWait(1); err != nil {
		return err
	}
	_ = e.flushCompletions()
	return nil
}

func (e *iouringState) runCtx(ctx context.Context, timeout time.Duration) error {
	ts := syscall.NsecToTimespec(int64(timeout))
	done := func() bool {
		select {
		case <-ctx.Done():
			return true
		default:
		}
		return false
	}
	if err := e.submit(); err != nil {
		return err
	}
	if _, err := e.ring.WaitCQEs(1, &ts, nil); err != nil && !TemporaryError(err) {
		return err
	}
	_ = e.flushCompletions()
	return nil
}

func (e *iouringState) runUntilDone() error {
	for {
		if e.callbacks.count() == 0 {
			if len(e.connections) > 0 || len(e.listeners) > 0 {
				panic("unclean shutdown")
			}
			return nil
		}
		if err := e.runOnce(); err != nil {
			return err
		}
	}
}

func (e *iouringState) apiPoll(tv time.Duration) (retVal int, err error) {
	if err := l.runCtx(ctx, time.Millisecond*333); err != nil {
		return err
	}
	l.closePendingConnections()
	// run loop until all operations finishes
	if err := l.runUntilDone(); err != nil {
		return err
	}
	return 0, nil
}

func (e *iouringState) delWrite(fd int) error {
	return nil
}

func (e *iouringState) apiName() string {
	return "io_uring"
}

func (e *iouringState) prepare(op operation) {
	sqe := e.ring.GetSQE()
	if sqe == nil { // submit and retry
		e.submit()
		sqe = e.ring.GetSQE()
	}
	if sqe == nil { // still nothing, add to pending
		e.pending = append(e.pending, op)
		return
	}
	op(sqe)
}

func (e *iouringState) submit() error {
	return e.submitAndWait(0)
}

func (e *iouringState) prepareRecv(fd int, cb completionCallback) {
	e.prepare(func(sqe *giouring.SubmissionQueueEntry) {
		sqe.PrepareRecvMultishot(fd, 0, 0, 0)
		sqe.Flags = giouring.SqeBufferSelect
		sqe.BufIG = buffersGroupID
		e.callbacks.set(sqe, cb)
	})
}

type ErrErrno struct {
	Errno syscall.Errno
}

func (e *ErrErrno) Error() string {
	return e.Errno.Error()
}

func (e *ErrErrno) Temporary() bool {
	o := e.Errno
	return o == syscall.EINTR || o == syscall.EMFILE || o == syscall.ENFILE ||
		o == syscall.ENOBUFS || e.Timeout()
}

func (e *ErrErrno) Timeout() bool {
	o := e.Errno
	return o == syscall.EAGAIN || o == syscall.EWOULDBLOCK || o == syscall.ETIMEDOUT ||
		o == syscall.ETIME
}

func (e *ErrErrno) Canceled() bool {
	return e.Errno == syscall.ECANCELED
}

func (e *ErrErrno) ConnectionReset() bool {
	return e.Errno == syscall.ECONNRESET || e.Errno == syscall.ENOTCONN
}

// TemporaryError returns true if syscall.Errno should be threated as temporary.
func TemporaryError(err error) bool {
	if errno, ok := err.(syscall.Errno); ok {
		return (&ErrErrno{Errno: errno}).Temporary()
	}
	if os.IsTimeout(err) {
		return true
	}
	return false
}
