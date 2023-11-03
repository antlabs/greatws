//go:build linux
// +build linux

package bigws

import (
	"io"
	"log/slog"
	"math"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/pawelgaczynski/giouring"
)

const (
	batchSize      = 128
	buffersGroupID = 0 // currently using only 1 provided buffer group
)

type (
	completionCallback = func(res int32, flags uint32, err *ErrErrno)
	operation          = func(*giouring.SubmissionQueueEntry)

	iouringState struct {
		ring        *giouring.Ring // ring 对象
		ringEntries uint32
		parent      *EventLoop
		callbacks   callbacks
		buffers     providedBuffers
		pending     []operation
	}
)

func apiIoUringCreate(el *EventLoop, ringEntries uint32) (la linuxApi, err error) {
	var iouringState iouringState
	ring, err := giouring.CreateRing(ringEntries)
	iouringState.ring = ring
	iouringState.parent = el
	iouringState.callbacks.init()
	iouringState.buffers.init(ring, 1000, 1000)
	return &iouringState, nil
}

func (e *iouringState) apiFree() {
	// TODO
	// e.closePendingConnections()
	// run loop until all operations finishes
	_ = e.runUntilDone()
}

func (e *iouringState) prepareShutdown(fd int, cb completionCallback) {
	e.prepare(func(sqe *giouring.SubmissionQueueEntry) {
		const SHUT_RDWR = 2
		sqe.PrepareShutdown(fd, SHUT_RDWR)
		// TODO
		e.callbacks.set(sqe, cb)
	})
}

func (e *iouringState) shutdown(err error, fd int) {
	if err == nil {
		panic("tcp conn missing shutdown reason")
	}
	// if e.shutdownError != nil {
	// 	return
	// }
	// e.shutdownError = err
	e.prepareShutdown(fd, func(res int32, flags uint32, err *ErrErrno) {
		if err != nil {
			if !err.ConnectionReset() {
				e.getLogger().Debug("tcp conn shutdown", "fd", fd, "err", err, "res", res, "flags", flags)
			}
			// TODO:: close fd
			return
		}

		e.prepareClose(fd, func(res int32, flags uint32, err *ErrErrno) {
			if err != nil {
				e.getLogger().Debug("tcp conn close", "fd", fd, "errno", err, "res", res, "flags", flags)
			}
			// TODO: close fd
			// e.Closed(tc.shutdownError)
		})
	})
}

type iouringConn struct {
	*iouringState
	fd int
}

func newIouringConn(e *iouringState, fd int) *iouringConn {
	return &iouringConn{iouringState: e, fd: fd}
}

func (e *iouringConn) Write(data []byte) (n int, err error) {
	nn := 0 // number of bytes sent
	var cb completionCallback
	var pinner runtime.Pinner
	pinner.Pin(&data[0])
	cb = func(res int32, flags uint32, err *ErrErrno) {
		nn += int(res) // bytes written so far
		if err != nil {
			pinner.Unpin()
			e.shutdown(err, e.fd)
			return
		}
		if nn >= len(data) {
			pinner.Unpin()
			// tc.up.Sent() // all sent call callback
			return
		}
		// send rest of the data
		e.prepareSend(e.fd, data[nn:], cb)
	}

	e.prepareSend(e.fd, data, cb)
	return len(data), nil
}

func (e *iouringState) addRead(c *Conn) (err error) {
	fd := c.getFd()
	// c := e.parent.parent.getConn(fd)
	c.w = newIouringConn(e, fd)

	var cb completionCallback
	cb = func(res int32, flags uint32, err *ErrErrno) {
		e.getLogger().Debug("addRead cb, err:%v", err.Error())
		if err != nil {
			if err.Temporary() {
				e.getLogger().Debug("tcp conn read temporary error", "error", err.Error())
				e.prepareRecv(fd, cb)
				return
			}
			if !err.ConnectionReset() {
				e.getLogger().Warn("tcp conn read error", "error", err.Error())
			}
			e.shutdown(err, fd)
			return
		}
		if res == 0 {
			e.shutdown(io.EOF, fd)
			return
		}
		buf, id := e.buffers.get(res, flags)

		e.getLogger().Debug("iouring:tcp conn read %d bytes", res)
		// TODO
		// tc.up.Received(buf)
		// 读取数据后，释放buffer
		e.buffers.release(buf, id)
		if !isMultiShot(flags) {
			e.getLogger().Debug("tcp conn multishot terminated", slog.Uint64("flags", uint64(flags)), slog.String("error", err.Error()))
			// io_uring can terminate multishot recv when cqe is full
			// need to restart it then
			// ref: https://lore.kernel.org/lkml/20220630091231.1456789-3-dylany@fb.com/T/#re5daa4d5b6e4390ecf024315d9693e5d18d61f10
			e.prepareRecv(fd, cb)
		}
	}
	e.prepareRecv(fd, cb)
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
				e.getLogger().Debug("ceq without userdata", "res", cqe.Res, "flags", cqe.Flags, "err", err)
				continue
			}
			cb := e.callbacks.get(cqe)
			cb(cqe.Res, cqe.Flags, err)
		}
		if peeked > 0 {
			e.getLogger().Debug("peeked", "peeded", peeked)
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
		e.getLogger().Error("submitAndWait(1)", "error", err.Error())
		return err
	}
	_ = e.flushCompletions()
	return nil
}

func (e *iouringState) getLogger() *slog.Logger {
	return e.parent.parent.Logger
}

func (e *iouringState) run(timeout time.Duration) error {
	ts := syscall.NsecToTimespec(int64(timeout))

	if err := e.submit(); err != nil {
		e.getLogger().Error("run.submit", "err", err.Error())
		return err
	}
	if _, err := e.ring.WaitCQEs(1, &ts, nil); err != nil && !TemporaryError(err) {
		e.getLogger().Error("run.WaitCQEs", "err", err.Error())
		return err
	}
	_ = e.flushCompletions()
	return nil
}

func (e *iouringState) runUntilDone() error {
	for {
		if e.callbacks.count() == 0 {
			return nil
		}
		if err := e.runOnce(); err != nil {
			return err
		}
	}
}

func (e *iouringState) apiPoll(tv time.Duration) (retVal int, err error) {
	if err := e.run(time.Millisecond * 333); err != nil {
		return 0, err
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

func (e *iouringState) preparePending() {
	prepared := 0
	for _, op := range e.pending {
		sqe := e.ring.GetSQE()
		if sqe == nil {
			break
		}
		op(sqe)
		prepared++
	}
	if prepared == len(e.pending) {
		e.pending = nil
	} else {
		e.pending = e.pending[prepared:]
	}
}

func (e *iouringState) submitAndWait(waitNr uint32) error {
	for {
		if len(e.pending) > 0 {
			_, err := e.ring.SubmitAndWait(0)
			if err == nil {
				e.preparePending()
			}
		}

		_, err := e.ring.SubmitAndWait(waitNr)
		if err != nil && TemporaryError(err) {
			continue
		}
		return err
	}
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

func (l *iouringState) prepareSend(fd int, buf []byte, cb completionCallback) {
	l.prepare(func(sqe *giouring.SubmissionQueueEntry) {
		sqe.PrepareSend(fd, uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf)), 0)
		l.callbacks.set(sqe, cb)
	})
}

func (l *iouringState) prepareClose(fd int, cb completionCallback) {
	l.prepare(func(sqe *giouring.SubmissionQueueEntry) {
		sqe.PrepareClose(fd)
		l.callbacks.set(sqe, cb)
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

type callbacks struct {
	m      map[uint64]completionCallback
	callNo uint64
}

func (c *callbacks) init() {
	c.m = make(map[uint64]completionCallback)
	c.callNo = math.MaxUint16 // reserve first few userdata values for internal use
}

func (c *callbacks) set(sqe *giouring.SubmissionQueueEntry, cb completionCallback) {
	newNo := atomic.AddUint64(&c.callNo, 1)
	c.m[newNo] = cb
	sqe.UserData = newNo
}

func (c *callbacks) get(cqe *giouring.CompletionQueueEvent) completionCallback {
	ms := isMultiShot(cqe.Flags)
	cb := c.m[cqe.UserData]
	if !ms {
		delete(c.m, cqe.UserData)
	}
	return cb
}

func (c *callbacks) count() int {
	return len(c.m)
}

func isMultiShot(flags uint32) bool {
	return flags&giouring.CQEFMore > 0
}

type providedBuffers struct {
	br      *giouring.BufAndRing
	data    []byte
	entries uint32
	bufLen  uint32
}

func (b *providedBuffers) init(ring *giouring.Ring, entries uint32, bufLen uint32) error {
	b.entries = entries
	b.bufLen = bufLen
	// mmap allocated space for all buffers
	var err error
	size := int(b.entries * b.bufLen)
	b.data, err = syscall.Mmap(-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		return err
	}
	// share buffers with io_uring
	b.br, err = ring.SetupBufRing(b.entries, buffersGroupID, 0)
	if err != nil {
		return err
	}
	for i := uint32(0); i < b.entries; i++ {
		b.br.BufRingAdd(
			uintptr(unsafe.Pointer(&b.data[b.bufLen*i])),
			b.bufLen,
			uint16(i),
			giouring.BufRingMask(b.entries),
			int(i),
		)
	}
	b.br.BufRingAdvance(int(b.entries))
	return nil
}

// get provided buffer from cqe res, flags
func (b *providedBuffers) get(res int32, flags uint32) ([]byte, uint16) {
	isProvidedBuffer := flags&giouring.CQEFBuffer > 0
	if !isProvidedBuffer {
		panic("missing buffer flag")
	}
	bufferID := uint16(flags >> giouring.CQEBufferShift)
	start := uint32(bufferID) * b.bufLen
	n := uint32(res)
	return b.data[start : start+n], bufferID
}

// return provided buffer to the kernel
func (b *providedBuffers) release(buf []byte, bufferID uint16) {
	b.br.BufRingAdd(
		uintptr(unsafe.Pointer(&buf[0])),
		b.bufLen,
		uint16(bufferID),
		giouring.BufRingMask(b.entries),
		0,
	)
	b.br.BufRingAdvance(1)
}

func (b *providedBuffers) deinit() {
	_ = syscall.Munmap(b.data)
}

func cqeErr(c *giouring.CompletionQueueEvent) *ErrErrno {
	if c.Res > -4096 && c.Res < 0 {
		errno := syscall.Errno(-c.Res)
		return &ErrErrno{Errno: errno}
	}
	return nil
}
