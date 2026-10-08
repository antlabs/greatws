// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/antlabs/wsutil/bytespool"
)

// 攒包(cork)缓冲区的大小。见 cork.go。
const maxCorkBytes = 32 * 1024

// 读缓冲区的下限(自适应增长那个), 见 growReadBuffer。
const batchReadBufferSize = 16 * 1024

var (
	ErrClosed = errors.New("engine: connection closed")
	// ErrWouldBlock 表示这次没写出去, 剩下的交给可写事件。
	ErrWouldBlock = errors.New("engine: would block")
)

// Conn 是一条非阻塞连接。
//
// **它只有传输层的东西**: fd、读缓冲区、写缓冲、状态位。任何和协议有关的
// （帧头、状态码、解析状态机）都在协议包自己的结构里——协议实现把它的
// 状态挂在 UserData 上（见 SetUserData）。
//
// 这样分的理由：epoll 的注册、部分写、EAGAIN 的处理和协议无关，写一遍就
// 够了；反过来，一个协议的状态机不该被别的协议看见。
type Conn struct {
	fd int64

	// 读缓冲区。rbuf[rr:rw] 是已经读到、还没被协议消费的数据。
	rbuf *[]byte
	rr   int // 读索引
	rw   int // 写索引

	// 写缓冲。直接写失败(部分写/EAGAIN)的剩余数据按顺序排在这里，
	// 可写事件到了再补写。
	wbufList []*[]byte

	// userData 是协议挂自己的状态。协议包在 OnOpen 里放, 在 OnClose 里
	// 别管——连接对象本身会被复用池回收。
	//
	// 用 atomic.Value 而不是普通字段: Add 是 accept 循环（调用方
	// goroutine）调的, 而 OnOpen 里设的这个值之后被事件循环读——两个
	// goroutine 一写一读就是竞争（-race 会报）。原型里早先是普通字段,
	// 实测就是这么炸的。
	userData atomic.Value // 存 any, 用 Load/Store

	// packed 把几个状态位压进一个 uint32:
	//
	//	bit 0      client
	//	bit 1      busy(这个连接正被某个 goroutine 处理)
	//	bit 2      pendingRead(处理期间又到了可读事件)
	//	bit 3      pendingWrite
	//	bit 4      corking(这一轮 read 里还有后续数据, 回包先攒着)
	packed uint32

	// 关连接只做一次
	closeOnce sync.Once
	closed    int32

	// handler 是这个连接的协议
	handler Handler
	// parent 是它挂在哪个事件循环上
	parent *EventLoop

	// mu 保护 rbuf/wbufList: Close 可能从任意 goroutine 来, 它要在锁里
	// 释放这两块内存。
	mu sync.Mutex
}

const (
	flagClient       uint32 = 1 << 0
	flagBusy         uint32 = 1 << 1
	flagPendingRead  uint32 = 1 << 2
	flagPendingWrite uint32 = 1 << 3
	flagCorking      uint32 = 1 << 4
)

// Init 初始化一条连接。fd 必须是已经设成非阻塞的 socket。
func (c *Conn) Init(fd int, h Handler, parent *EventLoop) {
	c.fd = int64(fd)
	c.handler = h
	c.parent = parent
	c.closed = 0
	c.packed = 0
	c.rbuf = nil
	c.rr, c.rw = 0, 0
	c.wbufList = c.wbufList[:0]
	// userData 是 atomic.Value，清成"没设过"（存一个 nil 指针）
	c.userData.Store((*any)(nil))
}

// Fd 返回文件描述符。连接关掉之后返回 -1。
func (c *Conn) Fd() int { return int(atomic.LoadInt64(&c.fd)) }

// SetUserData 让协议挂自己的状态(解析器、握手上下文...)。
//
// 可以在任意 goroutine 上调（内部用 atomic.Value）。协议通常在 OnOpen
// 里设、在 OnData 里读，而这两者可能在不同的 goroutine 上。
func (c *Conn) SetUserData(v any) {
	if v == nil {
		// atomic.Value 不允许存 nil
		c.userData.Store((*any)(nil))
		return
	}
	c.userData.Store(&v)
}

// UserData 取协议挂的状态。没设过返回 nil。
func (c *Conn) UserData() any {
	p := c.userData.Load()
	if p == nil {
		return nil
	}
	// 存的是 *any。Load 返回的接口里包着这个指针；没 Store 过的话
	// p 是 nil（不是 (*any)(nil)，是接口本身为 nil），上面那行拦住了。
	pp, ok := p.(*any)
	if !ok || pp == nil {
		return nil
	}
	return *pp
}

// IsClosed 连接关了没有。
func (c *Conn) IsClosed() bool { return atomic.LoadInt32(&c.closed) == 1 }

// ---------------------------------------------------------------------------
// 读

// Read 把 fd 上现成的数据读到读缓冲区, 返回这次读到多少。
//
// 返回 0 且 err == nil 表示这次没数据了(EAGAIN)。返回 io.EOF 表示对端
// 关了。读到的东西在 ReadBuffer() 里, 协议自己消费。
//
// 这个方法由引擎在读事件里调, 协议不直接调它——协议实现 OnData 拿到的
// 就是这里读进来的数据。
func (c *Conn) Read() (int, error) {
	if c.IsClosed() {
		return 0, ErrClosed
	}
	if c.rbuf == nil {
		c.rbuf = bytespool.GetBytes(8 * 1024)
	}

	total := 0
	for {
		buf := (*c.rbuf)[c.rw:]
		if len(buf) == 0 {
			// 缓冲区满了。协议还没消费完, 先把缓冲区加大——不能在这里
			// 丢掉没消费的数据。
			if !c.growReadBuffer() {
				return total, nil
			}
			continue
		}

		// 这把锁要拿着: Close 可能从任意 goroutine 来, 它会在锁里关掉
		// fd、释放读缓冲区。不拿锁读就会读到已关闭的 fd。
		c.mu.Lock()
		fd := int(atomic.LoadInt64(&c.fd))
		n, err := socketRead(fd, buf)
		c.mu.Unlock()

		if err != nil {
			if errno, ok := err.(syscall.Errno); ok {
				if errno == syscall.EINTR {
					continue
				}
				if errno == syscall.EAGAIN || errno == syscall.EWOULDBLOCK {
					return total, nil
				}
				return total, err
			}
			return total, err
		}
		if n == 0 {
			// 对端关了(FIN)。缓冲区里还有没消费的数据的话, 先让协议
			// 消费完再报 EOF——协议那边可能还有半条报文要处理。全消费
			// 完了就直接报 io.EOF, 让引擎关连接。
			if c.rw > c.rr {
				return total, nil
			}
			return total, io.EOF
		}

		c.rw += n
		total += n

		// 一次读满了说明后面还有, 换块大的, 免得下一条报文只读到一半。
		if c.rw == len(*c.rbuf) && len(*c.rbuf) < batchReadBufferSize {
			c.growReadBuffer()
		}

		// 短读**不能**直接返回。ET 模式下"数据读完"和"对端关了(FIN)"
		// 是两个独立的状态变化，而边缘只在后者到来时给一次——如果这次
		// 只读到数据就返回，FIN 那个边缘会因为"这个 fd 已经在处理中"
		// 被并进 pending，而 pending 的处理不会再产生新的读……
		//
		// 实测：客户端发 5 字节再 close，只有一次 [poll] 事件，OnClose
		// 永远不调。所以要接着读，直到 EAGAIN（没数据了）或者 0（FIN）。
		if n < len(buf) {
			continue
		}
	}
}

// ReadBuffer 返回已经读到、还没被消费的数据。
//
// 返回的切片只在协议处理这一轮里有效——下一次 Read 会追加/覆盖它。
func (c *Conn) ReadBuffer() []byte {
	if c.rbuf == nil {
		return nil
	}
	return (*c.rbuf)[c.rr:c.rw]
}

// ConsumeRead 告诉引擎协议消费了多少字节。
//
// 协议在 OnData 里返回消化量时引擎会自己调, 手写的协议循环里也能自己调。
func (c *Conn) ConsumeRead(n int) {
	if n <= 0 {
		return
	}
	c.rr += n
	if c.rr > c.rw {
		c.rr = c.rw
	}
	// 全消费完了就整块还回池子(下一次 Read 再取)
	if c.rr == c.rw {
		c.mu.Lock()
		if c.rbuf != nil {
			bytespool.PutBytes(c.rbuf)
			c.rbuf = nil
		}
		c.rr, c.rw = 0, 0
		c.mu.Unlock()
	}
}

// maxReadBufferSize 是读缓冲区能长到多大。
//
// 为什么要有这个数而不是"无限长": 缓冲区是**每个连接一块**，长到多大
// 就占多大内存（10000 连接 × 1MB = 10GB）。所以给它一个上限，超了就
// 不再收——但那意味着那条连接会卡住（见 Read 里的说明），所以这个值
// 要明显大于"正常一条报文能有多大"。
//
// 4MB：比常见的大 body（上传文件、gRPC 消息）都大，又不至于一条连接
// 吃掉太多内存。
const maxReadBufferSize = 4 * 1024 * 1024

// growReadBuffer 把读缓冲区换大。返回是否换成了。
//
// 生长分两段:
//
//	< 16KB(batchReadBufferSize)  一次跳到 16KB。目的不是"装更多数据",
//	                             是"一次读能把一个批次读完"(见
//	                             batchReadBufferSize 的注释)
//	>= 16KB                      翻倍。这是"协议还没消费完、缓冲区就满了"
//	                             那条路, 必须长——不然会卡死(见下)
//
// **翻倍这段是必须的, 而且曾经漏掉过**: 早先这里写的是"到了 16KB 就
// 不再长", 结果是——一个 32KB 的 HTTP body, 缓冲区被填满、解析器还在
// 等剩下的数据、又没有空间读新的 → Read 每次都返回 0 字节, 连接永久
// 卡住。自己写的测试撞不到(那些 body 都在一次 read 里能装下), 是拿
// 标准库的 http.Client 打 64KB POST 才打出来的。
func (c *Conn) growReadBuffer() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rbuf == nil {
		return false
	}
	cur := len(*c.rbuf)
	if cur >= maxReadBufferSize {
		return false
	}

	var want int
	if cur < batchReadBufferSize {
		want = batchReadBufferSize
	} else {
		want = cur * 2 // 翻倍
		if want > maxReadBufferSize {
			want = maxReadBufferSize
		}
	}

	old := c.rbuf
	nb := bytespool.GetBytes(want)
	copy(*nb, (*old)[:c.rw])
	c.rbuf = nb
	bytespool.PutBytes(old)
	return true
}

// ---------------------------------------------------------------------------
// 写

// Write 把 data 写出去。写不完的部分自动进写缓冲, 可写事件到了补写。
//
// 返回 error 只有两种情况: 连接已关, 或者写出了一个真错误。
func (c *Conn) Write(data []byte) error {
	if c.IsClosed() {
		return ErrClosed
	}
	if len(data) == 0 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 有积压: 先拼到后面去, 保证顺序
	if len(c.wbufList) > 0 {
		c.appendToWbufList(data, len(data))
		c.flushLocked()
		return nil
	}

	n, err := c.writeToSocket(data)
	if err == nil && n == len(data) {
		return nil
	}
	if err == nil || err == syscall.EAGAIN || err == syscall.EINTR {
		// 部分写: 剩下的攒起来, 等可写事件
		if n < len(data) {
			c.appendToWbufList(data[n:], len(data)-n)
		}
		c.parent.addWrite(c)
		return nil
	}
	return err
}

// Writev 写多段(header + payload 这类), 不拷成一个块。
//
// 段数上限是 2: 内核的 iovec 支持更多, 但攒包路径只用得到两段; 需要更多
// 的时候调用方自己拼。
func (c *Conn) Writev(a, b []byte) error {
	if c.IsClosed() {
		return ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.wbufList) > 0 {
		// 有积压, 拼起来走普通那条
		all := make([]byte, 0, len(a)+len(b))
		all = append(all, a...)
		all = append(all, b...)
		c.appendToWbufList(all, len(all))
		c.flushLocked()
		return nil
	}

	n, err := socketWritev(int(c.fd), a, b)
	total := len(a) + len(b)
	if err == nil && n == total {
		return nil
	}
	if err == nil || err == syscall.EAGAIN || err == syscall.EINTR {
		if n < 0 {
			n = 0
		}
		rest := make([]byte, 0, total-n)
		if n < len(a) {
			rest = append(rest, a[n:]...)
			rest = append(rest, b...)
		} else {
			rest = append(rest, b[n-len(a):]...)
		}
		c.appendToWbufList(rest, total)
		c.parent.addWrite(c)
		return nil
	}
	return err
}

// Flush 把写缓冲里的东西尽量写出去。可写事件到了由引擎调。
func (c *Conn) Flush() error {
	if c.IsClosed() {
		return ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.flushLocked()
}

// NeedFlush 写缓冲里有没有东西。
func (c *Conn) NeedFlush() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.wbufList) > 0
}

func (c *Conn) flushLocked() error {
	i := 0
	for i < len(c.wbufList) {
		wbuf := c.wbufList[i]
		n, err := c.writeToSocket(*wbuf)
		if err == nil && n == len(*wbuf) {
			bytespool.PutBytes(wbuf)
			c.wbufList[i] = nil
			i++
			continue
		}
		if err == nil || err == syscall.EAGAIN || err == syscall.EINTR {
			if n > 0 {
				copy(*wbuf, (*wbuf)[n:])
				*wbuf = (*wbuf)[:len(*wbuf)-n]
			}
			// 没写完, 剩下的留在列表里
			copy(c.wbufList, c.wbufList[i:])
			c.wbufList = c.wbufList[:len(c.wbufList)-i]
			c.parent.addWrite(c)
			return nil
		}
		return err
	}
	c.wbufList = c.wbufList[:0]
	return nil
}

// writeToSocket 直接写。**调用方必须持有 c.mu**——这个方法在 Write、
// Writev、flushLocked 里被调，它们都在锁里。
//
// 早先这里自己 Lock 了一次，而 Write 已经持锁，直接死锁（实测：
// 一条连接收到第一个字节就卡住，测试 90 秒超时）。fd 用原子读，不用锁。
func (c *Conn) writeToSocket(data []byte) (int, error) {
	return socketWrite(int(atomic.LoadInt64(&c.fd)), data)
}

// appendToWbufList 把 data 追加到写缓冲。调用方持有 mu。
func (c *Conn) appendToWbufList(data []byte, oldLen int) {
	if len(data) == 0 {
		return
	}
	if len(c.wbufList) == 0 {
		nb := bytespool.GetBytes(len(data) + oldLen)
		copy(*nb, data)
		*nb = (*nb)[:len(data)]
		c.wbufList = append(c.wbufList, nb)
		return
	}
	last := c.wbufList[len(c.wbufList)-1]
	if cap(*last)-len(*last) >= len(data) {
		*last = append(*last, data...)
		return
	}
	nb := bytespool.GetBytes(len(data) + oldLen)
	copy(*nb, data)
	*nb = (*nb)[:len(data)]
	c.wbufList = append(c.wbufList, nb)
}

// ---------------------------------------------------------------------------
// 关

// Close 关连接。幂等。
func (c *Conn) Close() error {
	c.closeWith(nil)
	return nil
}

func (c *Conn) closeWith(err error) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}
	c.closeOnce.Do(func() {
		atomic.StoreInt32(&c.closed, 1)

		c.mu.Lock()
		fd := int(atomic.LoadInt64(&c.fd))
		atomic.StoreInt64(&c.fd, -1)
		if c.rbuf != nil {
			bytespool.PutBytes(c.rbuf)
			c.rbuf = nil
		}
		for i := range c.wbufList {
			if c.wbufList[i] != nil {
				bytespool.PutBytes(c.wbufList[i])
				c.wbufList[i] = nil
			}
		}
		c.wbufList = c.wbufList[:0]
		c.rr, c.rw = 0, 0
		c.mu.Unlock()

		if c.parent != nil {
			c.parent.del(c)
		}
		if fd >= 0 {
			closeFd(fd)
		}
		if c.handler != nil {
			c.handler.OnClose(c, err)
		}
	})
}

// ---------------------------------------------------------------------------
// 状态位(引擎内部用)

func (c *Conn) setClient(v bool) {
	if v {
		atomic.OrUint32(&c.packed, flagClient)
	} else {
		atomic.AndUint32(&c.packed, ^flagClient)
	}
}

func (c *Conn) isClient() bool { return atomic.LoadUint32(&c.packed)&flagClient != 0 }

func (c *Conn) tryBusy() bool {
	return atomic.OrUint32(&c.packed, flagBusy)&flagBusy == 0
}

func (c *Conn) unbusy() { atomic.AndUint32(&c.packed, ^flagBusy) }

func (c *Conn) setPendingRead()  { atomic.OrUint32(&c.packed, flagPendingRead) }
func (c *Conn) setPendingWrite() { atomic.OrUint32(&c.packed, flagPendingWrite) }

func (c *Conn) takePendingRead() bool {
	return atomic.AndUint32(&c.packed, ^flagPendingRead)&flagPendingRead != 0
}

func (c *Conn) takePendingWrite() bool {
	return atomic.AndUint32(&c.packed, ^flagPendingWrite)&flagPendingWrite != 0
}

func (c *Conn) isCorking() bool { return atomic.LoadUint32(&c.packed)&flagCorking != 0 }

func (c *Conn) setCorking(v bool) {
	if v {
		atomic.OrUint32(&c.packed, flagCorking)
	} else {
		atomic.AndUint32(&c.packed, ^flagCorking)
	}
}
