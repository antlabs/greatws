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

package greatws

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antlabs/wsutil/bytespool"
	"github.com/antlabs/wsutil/enum"
	"github.com/antlabs/wsutil/errs"
	"github.com/antlabs/wsutil/fixedwriter"
	"github.com/antlabs/wsutil/frame"
	"github.com/antlabs/wsutil/mask"
	"github.com/antlabs/wsutil/opcode"
)

const (
	maxControlFrameSize = 125
)

type frameState int8

func (f frameState) String() string {
	switch f {
	case frameStateHeaderStart:
		return "frameStateHeaderStart"
	case frameStateHeaderPayloadAndMask:
		return "frameStateHeaderPayloadAndMask"
	case frameStatePayload:
		return "frameStatePayload"
	}
	return ""
}

const (
	frameStateHeaderStart frameState = iota
	frameStateHeaderPayloadAndMask
	frameStatePayload
)

// 内部的conn, 只包含fd, 读缓冲区, 写缓冲区, 状态机, 分段帧缓冲区
// 这一层本来是和epoll/kqueue 等系统调用打交道的
type conn struct {
	fd                   int64              // 文件描述符fd
	rbuf                 *[]byte            // 读缓冲区
	rr                   int                // rbuf读索引，rfc标准里面有超过4个字节的大包，所以索引只能用int类型
	rw                   int                // rbuf写索引，rfc标准里面有超过4个字节的大包，所以索引只能用int类型
	wbufList             []*[]byte          // 写缓冲区, 当直接Write失败时，会将数据写入缓冲区
	lenAndMaskSize       int                // payload长度和掩码的长度
	rh                   frame.FrameHeader  // frame头部
	fragmentFramePayload *[]byte            // 存放分片帧的缓冲区, TODO: 这个可以优化下 把Test_DefaultCallback和 fragmentFrameHeader 放到一个结构体里面
	fragmentFrameHeader  *frame.FrameHeader // 存放分段帧的头部
	// curState / client / busy 压进一个 uint32:
	//
	//	bit 0-1  curState(状态机, 只有 3 个值)
	//	bit 2    客户端为 1, 服务端为 0
	//	bit 3    busy, 这个连接正被某个 goroutine 处理
	//
	// 前两个原来是 int8 + bool, 占 96/97 两个字节, 后面还有 6 字节填充;
	// Conn 有 216 字节上限(conn_test.go 守着), 加不了新字段。busy 是
	// "分片里有按需帮手时, 同一连接不被两个 goroutine 同时碰"的前提
	// (见 task_parse.go 的 helpOne)。
	packed uint32
}

const (
	stateMask  uint32 = 0x3
	flagClient uint32 = 1 << 2
	flagBusy   uint32 = 1 << 3
	// 处理期间又到了可读/可写事件。投递方发现 busy 已被占(说明有人在处理
	// 这个连接)时, 不重复投任务, 只把事件记在这里; 正在处理的那个跑完
	// 会取走它们再跑一轮。
	//
	// 为什么不能直接丢: ET 的边缘只来一次, 丢了就再也没有通知, 连接卡住
	// (实测: 随机分片 + 只加 busy 位的版本, 服务端从 2,087,688 TPS 掉到
	// 181,995)。
	flagPendingRead  uint32 = 1 << 4
	flagPendingWrite uint32 = 1 << 5
	// flagCorking: 这一轮 read 里还有后续 frame, 回调写出去的消息先攒在
	// wbufList 里, 轮末一次写出去。见 cork.go。
	flagCorking uint32 = 1 << 6
)

// 下面几个都走原子: packed 里既有"只有本 goroutine 碰"的状态位
// (curState), 也有跨 goroutine 的位(busy/client)。Go 的 atomic 在 x86 上
// 就是普通 load/store(带编译器屏障), 所以统一用原子不会变慢, 反而避免了
// 非原子读改写把别的 goroutine 原子置的位覆盖掉。
func (c *Conn) getCurState() frameState {
	return frameState(atomic.LoadUint32(&c.packed) & stateMask)
}

func (c *Conn) setCurState(st frameState) {
	for {
		old := atomic.LoadUint32(&c.packed)
		nv := (old &^ stateMask) | (uint32(st) & stateMask)
		if atomic.CompareAndSwapUint32(&c.packed, old, nv) {
			return
		}
	}
}

func (c *Conn) isClient() bool { return atomic.LoadUint32(&c.packed)&flagClient != 0 }

func (c *Conn) setClient(v bool) {
	if v {
		atomic.OrUint32(&c.packed, flagClient)
	} else {
		atomic.AndUint32(&c.packed, ^flagClient)
	}
}

// tryBusy 在"这个连接没人在处理"时把 busy 置上, 返回是否抢到。
func (c *Conn) tryBusy() bool {
	return atomic.OrUint32(&c.packed, flagBusy)&flagBusy == 0
}

// unbusy 交回 busy。
func (c *Conn) unbusy() { atomic.AndUint32(&c.packed, ^flagBusy) }

func (c *Conn) getLogger() *slog.Logger {
	return c.multiEventLoop.Logger
}

// addTask 把回调交给连接的任务执行器。
//
// io 模式的 task 是 nil: 那个模式就是"就地执行", 而它占了这个库绝大
// 多数部署(包括默认配置)。走接口要过一次动态派发加一层函数调用, 而
// 这是每条消息至少一次的路径, 所以让它直接调。
func (c *Conn) addTask(f func() bool) {
	if c.isClosed() {
		return
	}

	if c.task == nil {
		f()
		return
	}

	err := c.task.AddTask(&c.mu, f)
	if err != nil {
		c.getLogger().Error("addTask", "err", err.Error())
	}
}

func (c *Conn) getFd() int {
	return int(atomic.LoadInt64(&c.fd))
}

// 基于状态机解析frame
func (c *Conn) readHeader() (sucess bool, err error) {
	state := c.getCurState()
	// 开始解析frame
	if state == frameStateHeaderStart {
		// 小于最小的frame头部长度, 有空间就挪一挪
		if len(*c.rbuf)-c.rr < enum.MaxFrameHeaderSize {
			c.leftMove()
		}
		// fin rsv1 rsv2 rsv3 opcode
		if c.rw-c.rr < 2 {
			return false, nil
		}
		c.rh.Head = (*c.rbuf)[c.rr]

		// h.Fin = head[0]&(1<<7) > 0
		// h.Rsv1 = head[0]&(1<<6) > 0
		// h.Rsv2 = head[0]&(1<<5) > 0
		// h.Rsv3 = head[0]&(1<<4) > 0
		c.rh.Opcode = opcode.Opcode(c.rh.Head & 0xF)

		maskAndPayloadLen := (*c.rbuf)[c.rr+1]
		have := 0
		c.rh.Mask = maskAndPayloadLen&(1<<7) > 0

		if c.rh.Mask {
			have += 4
		}

		c.rh.PayloadLen = int64(maskAndPayloadLen & 0x7F)
		switch {
		// 长度
		case c.rh.PayloadLen >= 0 && c.rh.PayloadLen <= 125:
		case c.rh.PayloadLen == 126:
			// 2字节长度
			have += 2
			// size += 2
		case c.rh.PayloadLen == 127:
			// 8字节长度
			have += 8
			// size += 8
		default:
			// 预期之外的, 直接报错
			return sucess, errs.ErrFramePayloadLength
		}
		c.setCurState(frameStateHeaderPayloadAndMask)
		state = frameStateHeaderPayloadAndMask
		c.lenAndMaskSize = have
		c.rr += 2

	}

	if state == frameStateHeaderPayloadAndMask {
		if c.rw-c.rr < c.lenAndMaskSize {
			return
		}
		have := c.lenAndMaskSize
		head := (*c.rbuf)[c.rr : c.rr+have]
		switch c.rh.PayloadLen {
		case 126:
			c.rh.PayloadLen = int64(binary.BigEndian.Uint16(head[:2]))
			head = head[2:]
		case 127:
			c.rh.PayloadLen = int64(binary.BigEndian.Uint64(head[:8]))
			head = head[8:]
		}

		if c.readMaxMessage > 0 && c.rh.PayloadLen > c.readMaxMessage {
			return false, TooBigMessage
		}

		if c.rh.Mask {
			c.rh.MaskKey = binary.LittleEndian.Uint32(head[:4])
		}
		c.setCurState(frameStatePayload)
		c.rr += c.lenAndMaskSize
		return true, nil
	}

	return state == frameStatePayload, nil
}

func (c *Conn) failRsv1(op opcode.Opcode) bool {
	// 解压缩没有开启
	if !c.pd.Decompression {
		return true
	}

	// 不是text和binary
	if op != opcode.Text && op != opcode.Binary {
		return true
	}

	return false
}

func (c *Conn) leftMove() {
	if c.rr == 0 {
		return
	}
	// b.CountMove++
	// b.MoveBytes += b.W - b.R
	n := copy(*c.rbuf, (*c.rbuf)[c.rr:c.rw])
	c.rw -= c.rr
	c.rr = 0
	c.multiEventLoop.addMoveBytes(uint64(n))
}

// readBufferSize 是读缓冲区的大小: 按上一条消息的 payload 算
// (windowsMultipleTimesPayloadSize 倍, 默认 2.0, 见 Config.defaultSetting),
// 留一倍余量, 免得帧头一多就要扩容。
func (c *Conn) readBufferSize() int {
	return int(float32(c.rh.PayloadLen)*c.windowsMultipleTimesPayloadSize) + enum.MaxFrameHeaderSize
}

// batchReadBufferSize 是"这条连接一次 read 能带一批消息"时读缓冲区抬到的
// 大小。
//
// 为什么需要: readBufferSize 那个算法假设"一次 read 拿一条消息", 而客户端
// 一次 write 可能带多条(压测的 Pipeline: -rpl 15, 一次 15480 字节)。缓冲区
// 只装得下一条时, 内核把整批给它, 它只解析出一条, 剩下的留在 socket 里;
// 这些数据已经不产生新的边缘了(ET 的边缘是"新数据到达"触发的), 只能等
// 下一次事件——于是变成一条消息一次 read。实测(A 配置, 服务端计数): 读系统
// 调用 2,000,000/s = 每条消息一次(写已经是每批一次 200,000/s); 抬上去之后
// 读也降到 200,000/s, CPU 344% -> 271%, CPU EER 5,810 -> 7,384(超过 fnet)。
//
// 16KB 是因为常见的一次写批次是 8~16KB(-rbs 16384 就是按这个定的)。
const batchReadBufferSize = 16 * 1024

// growReadBuffer 把读缓冲区换成 batchReadBufferSize 大小, 已经读进来的
// 数据跟着挪过去。
//
// 只在读循环里"刚读完、还没开始解析"那一刻调用: 那时候没有任何 payload
// 别名指向这块缓冲区(零拷贝的别名只活在回调那次调用里, 见
// WithServerZeroCopyPayload), 换掉它是安全的。解析中途换会把自己正在用
// 的那块内存还回池子。
func (c *Conn) growReadBuffer() {
	old := c.rbuf
	nb := bytespool.GetBytes(batchReadBufferSize)
	copy(*nb, (*old)[:c.rw])
	c.rbuf = nb
	bytespool.PutBytes(old)
	c.multiEventLoop.addRealloc()
}

func (c *Conn) writeCap() int {
	return len((*c.rbuf)[c.rw:])
}

// 需要考虑几种情况
// 返回完整Payload逻辑
// 1. 当前的rbuf长度不够，需要重新分配
// 2. 当前的rbuf长度够，但是数据没有读完整
// 返回分片Paylod逻辑
// TODO
//
// needCopy 为 false 时 payload 直接指向 rbuf, 不拷也不从池里取内存。
// 调用方保证这块内存只在本次回调里用(见 WithServerZeroCopyPayload)。
func (c *Conn) readPayload(needCopy bool) (f frame.Frame2, success bool, err error) {
	// 如果缓存区不够, 重新分配
	multipletimes := c.windowsMultipleTimesPayloadSize
	// 已读取未处理的数据
	readUnhandle := int64(c.rw - c.rr)
	// 情况 1，需要读的长度 > 剩余可用空间(未写的+已经被读取走的)
	if c.rh.PayloadLen-readUnhandle > int64(len((*c.rbuf)[c.rw:])+c.rr) {
		// 1.取得旧的buf
		oldBuf := c.rbuf
		// 2.获取新的buf
		newBuf := bytespool.GetBytes(int(float32(c.rh.PayloadLen)*multipletimes) + enum.MaxFrameHeaderSize)
		// 把旧的数据拷贝到新的buf里
		copy(*newBuf, (*oldBuf)[c.rr:c.rw])
		c.rw -= c.rr
		c.rr = 0

		// 3.重置缓存区
		c.rbuf = newBuf
		// 4.将旧的buf放回池子里
		bytespool.PutBytes(oldBuf)
		c.multiEventLoop.addRealloc()

		// 情况 2。 空间是够的，需要挪一挪, 把已经读过的覆盖掉
	} else if c.rh.PayloadLen-readUnhandle > int64(c.writeCap()) {
		c.leftMove()
	}

	// 前面的reset已经保证了，buffer的大小是够的
	needRead := c.rh.PayloadLen - readUnhandle

	// fmt.Printf("needRead:%d:rr(%d):rw(%d):PayloadLen(%d), %v\n", needRead, c.rr, c.rw, c.rh.PayloadLen, c.rbuf)
	if needRead > 0 {
		return
	}
	// 普通frame
	if !needCopy {
		// payload 就是 rbuf 里这一段, 别名过去, 不分配也不拷贝。
		//
		// 用 copy 而不是 unsafe.Slice: 同一个起点、长度和容量都取
		// 一致时 copy 不会真的搬数据(实测 0 次 memmove), 但它是普通
		// 的切片表达式, 不需要 unsafe, 也没那么多坑。
		//
		// 注意这里 rr 必须照常推进: 数据在 rbuf 里, 但所有权已经算
		// 交出去了, 后面的解析不能再看它。回调返回后这块内存随
		// rbuf 一起复用。
		payload := (*c.rbuf)[c.rr : c.rr+int(c.rh.PayloadLen) : c.rr+int(c.rh.PayloadLen)]
		f.Payload = &payload
		f.FrameHeader = c.rh
		c.rr += int(c.rh.PayloadLen)
		// 别在这里 leftMove: 那会把 rbuf 里刚别名出去的那段搬走,
		// 回调读到的东西跟着变。空间够不够下一次再说, 下一次
		// readPayload 开头会自己判断。
		return f, true, nil
	}

	newBuf := bytespool.GetBytes(int(c.rh.PayloadLen) + enum.MaxFrameHeaderSize)
	copy(*newBuf, (*c.rbuf)[c.rr:c.rr+int(c.rh.PayloadLen)])
	newBuf2 := (*newBuf)[:c.rh.PayloadLen] //修改下len
	f.Payload = &newBuf2

	f.FrameHeader = c.rh
	c.rr += int(c.rh.PayloadLen)

	if len(*c.rbuf)-c.rw < enum.MaxFrameHeaderSize {
		c.leftMove()
	}

	return f, true, nil
}

// takePayload 把 payload 变成一块调用方可以长期持有的内存。
//
// needCopy 为 false 时 payload 只是读缓冲区的一段别名, 下一次 read 就会
// 覆盖它, 所以要拷进池里的一块新内存; 否则 payload 本来就是单独分配出来
// 的, 把所有权转过去就行, 不动数据。
//
// 分片消息用它: 第一个分片要留到最后一个分片到达, 中间隔着很多次 read。
func takePayload(p *[]byte, needCopy bool) *[]byte {
	if !needCopy {
		buf := bytespool.GetBytes(len(*p) + enum.MaxFrameHeaderSize)
		copy(*buf, *p)
		nb := (*buf)[:len(*p)]
		return &nb
	}
	return p
}

// putPayload 归还 payload; 零拷贝的那份是读缓冲区的别名, 不在池子里,
// 还回去会污染内存池, 所以按 needCopy 区分。
func putPayload(p *[]byte, needCopy bool) {
	if needCopy {
		bytespool.PutBytes(p)
	}
}

// needCopy 是 readPayload 给的: false 表示 f.Payload 是读缓冲区的一段
// 别名, 回调返回之后就不算数了, 所以后面凡是把 payload 存下来或者交给
// 别的 goroutine 的地方都必须自己拷一份(见下面分片和入池那两处)。
func (c *Conn) processCallback(f frame.Frame2, needCopy bool) (err error) {
	op := f.Opcode
	if c.fragmentFrameHeader != nil {
		op = c.fragmentFrameHeader.Opcode
	}

	rsv1 := f.GetRsv1()
	// 检查Rsv1 rsv2 Rfd, errsv3
	if rsv1 && c.failRsv1(op) || f.GetRsv2() || f.GetRsv3() {
		err = fmt.Errorf("%w:Rsv1(%t) Rsv2(%t) rsv2(%t) compression:%t", ErrRsv123, rsv1, f.GetRsv2(), f.GetRsv3(), c.pd.Compression)
		return c.writeErrAndOnClose(ProtocolError, err)
	}

	maskKey := c.rh.MaskKey
	needMask := c.rh.Mask

	fin := f.GetFin()
	// 分段的frame
	if c.fragmentFrameHeader != nil && !f.Opcode.IsControl() {
		if f.Opcode == 0 {
			// TODO 优化, 需要放到单独的业务go程, 目前为了保证时序性，先放到io go程里面
			if needMask {
				mask.Mask(*f.Payload, maskKey)
			}

			// 这里要留到最后一个分片到达, 中间隔着若干次 read, 所以
			// 零拷贝那份别名必须转成自己的一块内存, 见 takePayload。
			payloadOwn := takePayload(f.Payload, needCopy)
			if c.fragmentFramePayload == nil {
				c.fragmentFramePayload = payloadOwn
			} else {
				*c.fragmentFramePayload = append(*c.fragmentFramePayload, *payloadOwn...)
				putPayload(payloadOwn, true) // 已经是自己的内存了, 按池里的还
			}

			f.Payload = nil

			// 分段的在这返回
			if fin {
				// 解压缩
				fragmentFrameHeader := c.fragmentFrameHeader
				fragmentFramePayload := c.fragmentFramePayload
				decompression := c.pd.Decompression
				c.fragmentFrameHeader = nil
				c.fragmentFramePayload = nil

				// 进入业务协程执行
				c.addTask(func() (exit bool) {
					if fragmentFrameHeader.GetRsv1() && decompression {
						tempBuf, err := c.decode(fragmentFramePayload)
						if err != nil {
							// return err
							c.closeWithLock(err)
							return false
						}

						// 回收这块内存到pool里面
						bytespool.PutBytes(fragmentFramePayload)
						fragmentFramePayload = tempBuf
					}
					// 这里的check按道理应该放到f.Fin前面， 会更符合rfc的标准, 前提是c.utf8Check修改成流式解析
					// TODO c.utf8Check 修改成流式解析
					if fragmentFrameHeader.Opcode == opcode.Text && !c.utf8Check(*fragmentFramePayload) {
						c.onCloseOnce.Do(&c.mu2, func() {
							c.Callback.OnClose(c, ErrTextNotUTF8)
						})
						// return ErrTextNotUTF8
						c.closeWithLock(nil)
						return false
					}

					c.Callback.OnMessage(c, fragmentFrameHeader.Opcode, *fragmentFramePayload)
					bytespool.PutBytes(fragmentFramePayload)
					return false
				})
			}
			return nil
		}

		c.writeErrAndOnClose(ProtocolError, ErrFrameOpcode)
		return ErrFrameOpcode
	}

	if f.Opcode == opcode.Text || f.Opcode == opcode.Binary {
		if !fin {
			prevFrame := f.FrameHeader
			// 第一次分段

			// TODO 放到单独的业务go程, 目前为了保证时序性，先放到io go程里面
			if needMask {
				mask.Mask(*f.Payload, maskKey)
			}
			if c.fragmentFramePayload == nil {
				// 正常是单独分配出来的, 转移下变量的所有权就行; 零拷贝
				// 时它只是 rbuf 的一段, 得先拷成自己的。
				c.fragmentFramePayload = takePayload(f.Payload, needCopy)
				f.Payload = nil
			}

			// 让fragmentFrame的Payload指向readBuf, readBuf 原引用直接丢弃
			c.fragmentFrameHeader = &prevFrame
			return
		}

		// var payloadPtr atomic.Pointer[[]byte]
		decompression := c.pd.Decompression
		payload := f.Payload
		f.Payload = nil
		// payloadPtr.Store(f.Payload)

		// 回调就地执行(c.task == nil, 见 addTask)时, 数据在这个栈帧里
		// 就用完, 可以接着用读缓冲区那段别名; 投进池子的回调活到别的
		// 时候, 必须持有自己的一块内存。压缩的消息要解压, 解压本来就
		// 产出新内存, 两条路都一样, 不用在这里分。
		if c.task == nil {
			// 闭包是纯开销: 每个消息堆分配一个, 只为了马上同步调用一次。
			//
			// 这里不能只是"分支里直接调用, 底下再留一个闭包版本"——
			// 逃逸分析是按函数做的, 只要本函数里任何一处捕获了 f, f
			// 这个参数就整个进堆, 走哪条分支都躲不掉(实测: 684MB, 全记
			// 在函数入口那一行)。所以闭包版本挪到单独的 noinline 函数
			// 里去, 让逃逸发生在它自己的栈帧里。
			if !c.isClosed() {
				c.processCallbackData(f, payload, rsv1, decompression, needMask, maskKey, needCopy)
			}
			return
		}

		// 交给池: 它可能过一会才跑, 那时候 rbuf 已经换了内容。
		payloadOwn := takePayload(payload, needCopy)
		f.Payload = payloadOwn
		c.addProcessCallbackTask(f, payloadOwn, rsv1, decompression, needMask, maskKey, true)
		return
	}

	if f.Opcode == Close || f.Opcode == Ping || f.Opcode == Pong {

		// 消息体的内容比较小，直接在io go程里面处理
		if needMask {
			mask.Mask(*f.Payload, maskKey)
		}
		//  对方发的控制消息太大
		if f.PayloadLen > maxControlFrameSize {
			c.writeErrAndOnClose(ProtocolError, ErrMaxControlFrameSize)
			return ErrMaxControlFrameSize
		}
		// Close, Ping, Pong 不能分片
		if !fin {
			c.writeErrAndOnClose(ProtocolError, ErrNOTBeFragmented)
			return ErrNOTBeFragmented
		}

		if f.Opcode == Close {
			if len(*f.Payload) == 0 {
				c.writeErrAndOnClose(NormalClosure, &CloseErrMsg{Code: NormalClosure})
				return nil
			}

			if len(*f.Payload) < 2 {
				return c.writeErrAndOnClose(ProtocolError, ErrClosePayloadTooSmall)
			}

			if !c.utf8Check((*f.Payload)[2:]) {
				return c.writeErrAndOnClose(ProtocolError, ErrTextNotUTF8)
			}

			code := binary.BigEndian.Uint16(*f.Payload)
			if !validCode(code) {
				return c.writeErrAndOnClose(ProtocolError, ErrCloseValue)
			}

			// 回敬一个close包
			if err := c.WriteTimeout(Close, *f.Payload, 2*time.Second); err != nil {
				return err
			}

			err = bytesToCloseErrMsg(*f.Payload)
			c.onCloseOnce.Do(&c.mu2, func() {
				c.Callback.OnClose(c, err)
			})
			return err
		}

		if f.Opcode == Ping {
			// 回一个pong包
			if c.replyPing {
				if err := c.WriteTimeout(Pong, *f.Payload, 2*time.Second); err != nil {
					c.onCloseOnce.Do(&c.mu2, func() {
						c.Callback.OnClose(c, err)
					})
					return err
				}
				// 进入业务协程执行
				payload := f.Payload
				// here
				c.addTask(func() bool {
					return c.processPing(f, payload)
				})
				return
			}
		}

		if f.Opcode == Pong && c.ignorePong {
			return
		}

		// 进入业务协程执行
		c.addTask(func() bool {
			c.Callback.OnMessage(c, f.Opcode, nil)
			return false
		})
		return
	}
	// 检查Opcode
	c.writeErrAndOnClose(ProtocolError, ErrOpcode)
	return ErrOpcode
}

func (c *Conn) processPing(f frame.Frame2, payload *[]byte) bool {
	c.Callback.OnMessage(c, f.Opcode, *payload)
	bytespool.PutBytes(payload)
	return false
}

// addProcessCallbackTask 是"把回调交给任务池"的那条路, 单独一个函数
// 是为了把捕获 f 的闭包隔离在这里: 逃逸分析按函数做, 留在
// processCallback 里会让它的 f 参数无论走不走池都进堆。
//
// noinline 是必要的, 否则内联回去就白隔离了。
//
//go:noinline
func (c *Conn) addProcessCallbackTask(f frame.Frame2, payload *[]byte, rsv1 bool, decompression bool, needMask bool, maskKey uint32, owned bool) {
	c.addTask(func() bool {
		return c.processCallbackData(f, payload, rsv1, decompression, needMask, maskKey, owned)
	})
}

// 如果是text或者binary的消息， 在这里调用OnMessage函数
//
// owned 表示 payload 是不是我们自己的一块内存(池里来的, 或者 takePayload
// 拷出来的): 是就归还在池里, 不是就只是读缓冲区的一段别名, 碰不得。
func (c *Conn) processCallbackData(f frame.Frame2, payload *[]byte, rsv1 bool, decompression bool, needMask bool, maskKey uint32, owned bool) (ok bool) {
	var err error
	if needMask {
		mask.Mask(*payload, maskKey)
	}
	decodePayload := payload
	if rsv1 && decompression {
		// 不分段的解压缩
		decodePayload, err = c.decode(payload)
		if err != nil {
			c.closeWithLock(err)
			putPayload(payload, owned)
			return false
		}
		defer bytespool.PutBytes(decodePayload)
	}

	if f.Opcode == opcode.Text {
		if !c.utf8Check(*decodePayload) {
			c.closeWithLock(nil)
			c.onCloseOnce.Do(&c.mu2, func() {
				c.Callback.OnClose(c, ErrTextNotUTF8)
			})
			return false
		}
	}

	c.Callback.OnMessage(c, f.Opcode, *decodePayload)
	putPayload(payload, owned)
	return false
}

func (c *Conn) writeAndMaybeOnClose(err error) error {
	var sc *StatusCode
	defer func() {
		c.onCloseOnce.Do(&c.mu2, func() {
			c.Callback.OnClose(c, err)
		})
	}()

	if errors.As(err, &sc) {
		if err := c.WriteTimeout(opcode.Close, sc.toBytes(), 2*time.Second); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conn) writeErrAndOnClose(code StatusCode, userErr error) error {
	defer func() {
		c.onCloseOnce.Do(&c.mu2, func() {
			c.Callback.OnClose(c, userErr)
		})
	}()
	if err := c.WriteTimeout(opcode.Close, code.toBytes(), 2*time.Second); err != nil {
		return err
	}

	return userErr
}

func (c *Conn) readPayloadAndCallback() (sucess bool, err error) {
	if c.getCurState() == frameStatePayload {
		// 这几种情况必须拷: 压缩的要拿去解压(结果跟读缓冲区生命周期
		// 无关, 但解压本身按 payload 的长度读, 拷与不拷收益一样, 统一
		// 走拷贝省得分叉); 分段消息的下一个分片到达时这块内存已经换了
		// 内容; 已经进入分段状态时更不用说。
		//
		// 其余情况(单帧、不压缩、消息在一次 read 里拿全)对回调来说
		// 只是"回调期间有效"的字节, 正好和读缓冲区共用一块。
		needCopy := !c.zeroCopyPayload ||
			c.rh.GetRsv1() ||
			!c.rh.GetFin() ||
			c.fragmentFrameHeader != nil
		f, success, err := c.readPayload(needCopy)
		if err != nil {
			c.getLogger().Error("readPayloadAndCallback.read payload err", "err", err.Error())
			return sucess, err
		}

		// fmt.Printf("read payload, success:%t, %v\n", success, f.Payload)
		if success {
			c.maybeCork()
			if err := c.processCallback(f, needCopy); err != nil {
				c.closeWithLock(err)
				return false, err
			}
			c.setCurState(frameStateHeaderStart)
			return true, err
		}
	}
	return false, nil
}

func (c *Conn) isClosed() bool {
	return atomic.LoadInt32(&c.closed) == 1
}

func (c *Conn) WriteMessage(op Opcode, writeBuf []byte) (err error) {
	if c.isClosed() {
		return ErrClosed
	}

	if op == opcode.Text {
		if !c.utf8Check(writeBuf) {
			return ErrTextNotUTF8
		}
	}

	rsv1 := c.pd.Compression && (op == opcode.Text || op == opcode.Binary)
	if rsv1 {
		writeBufPtr, err := c.encoode(&writeBuf)
		if err != nil {
			return err
		}

		defer bytespool.PutBytes(writeBufPtr)
		writeBuf = *writeBufPtr
	}

	maskValue := uint32(0)
	if c.isClient() {
		maskValue = rand.Uint32()
	}

	var fw fixedwriter.FixedWriter
	_ = fw

	// 这把锁必须拿着: 它不只是给"回调被投到线程池"那个模式用的——
	// Close() 可能从任意 goroutine 来(用户代码、超时定时器), 它会在锁里
	// 释放 wbufList(见 conn_unix.go 的 closeWithLock), 不拿锁写缓冲区
	// 就会写到已释放的内存上。
	//
	// 实测(io 模式, 1KB echo, 交替 3 轮): 去掉这把锁 TPS 差 0.3%(噪声内)、
	// TP99 好 1.8%。收益是零, 不值得拿这个风险换。
	c.mu.Lock()
	defer c.mu.Unlock()

	// 攒包期间(一轮 read 里有多个 frame, 见 cork.go): 回包先进缓冲区,
	// 轮末一次写出去。这是 Pipeline 场景写路径 CPU 的主要来源。
	if c.isCorking() && !rsv1 {
		return c.corkWrite(uint8(op), writeBuf)
	}

	// io 模式 + 服务端 + 没压缩 + 长度放得进 2/4 字节头 + 没有积压:
	// 走 writev, header 在栈上拼, payload 不拷贝直接交给内核。
	//
	// WriteFrame 那条路每条消息多一次 1KB 的 memcpy(它要把 payload 拷进
	// 池里取出的 buf 再整块写)。fnet 用的就是这个(sendmsg + iovec)。
	//
	// 实测(io 模式, 1KB echo, 交替 3 轮): TPS 差 0.24%(噪声内),
	// TP95 好 1%(三轮全赢)、TP99 好 1%(三轮全赢)。收益很小但一致。
	// 4KB 以下拼成一块用 sendto, 和 fnet 的 writeFrame 一样
	// (maxCopiedPayload = 4KB)。
	//
	// sendmsg 要读 iovec 数组、sendto 只要一个指针, 小消息下后者更便宜;
	// 大消息才值得用 iovec 省那次拷贝。之前无条件用 sendmsg, 测下来小消息
	// 多花 CPU。
	if c.task == nil && !c.isClient() && !rsv1 && len(writeBuf) <= 4096 && len(c.wbufList) == 0 {
		var hdr [10]byte
		hn := wsHeader(hdr[:], uint8(op), len(writeBuf))
		var seg bufseg
		all := seg.alloc(hn, len(writeBuf))
		copy(all, hdr[:hn])
		copy(all[hn:], writeBuf)
		n, werr := socketWrite(c.getFd(), all)
		seg.free()
		c.addWriteSyscall()
		if werr == nil && n == len(all) {
			return nil
		}
		if werr == nil || werr == syscall.EAGAIN || werr == syscall.EINTR {
			c.appendToWbufList(all[n:], len(all)-n)
			if err := c.eventLoop().addWrite(c); err != nil {
				return err
			}
			return nil
		}
		return werr
	}
	if c.task == nil && !c.isClient() && !rsv1 && len(writeBuf) <= 65535 && len(c.wbufList) == 0 {
		var hdr [10]byte
		hn := wsHeader(hdr[:], uint8(op), len(writeBuf))
		n, werr := socketWritev(c.getFd(), hdr[:hn], writeBuf)
		c.addWriteSyscall()
		if werr == nil && n == hn+len(writeBuf) {
			return nil
		}
		if werr == nil || werr == syscall.EAGAIN || werr == syscall.EINTR {
			// 部分写: 把没写出去的拼起来进缓冲区, 剩下的交给可写事件
			all := make([]byte, 0, hn+len(writeBuf))
			all = append(all, hdr[:hn]...)
			all = append(all, writeBuf...)
			c.appendToWbufList(all[n:], len(all)-n)
			if err := c.eventLoop().addWrite(c); err != nil {
				return err
			}
			return nil
		}
		return werr
	}

	return frame.WriteFrame(&fw, connToNewConn(c), writeBuf, true, rsv1, c.isClient(), op, maskValue)
}

// 写分段数据, 目前主要是单元测试使用
func (c *Conn) writeFragment(op Opcode, writeBuf []byte, maxFragment int /*单个段最大size*/) (err error) {
	if len(writeBuf) < maxFragment {
		return c.WriteMessage(op, writeBuf)
	}

	if op == opcode.Text {
		if !c.utf8Check(writeBuf) {
			return ErrTextNotUTF8
		}
	}

	rsv1 := c.pd.Compression && (op == opcode.Text || op == opcode.Binary)
	if rsv1 {
		writeBufPtr, err := c.encoode(&writeBuf)
		if err != nil {
			return err
		}
		defer bytespool.PutBytes(writeBufPtr)
		writeBuf = *writeBufPtr
	}

	// f.Opcode = op
	// f.PayloadLen = int64(len(writeBuf))
	maskValue := uint32(0)
	if c.isClient() {
		maskValue = rand.Uint32()
	}

	var fw fixedwriter.FixedWriter
	_ = fw
	for len(writeBuf) > 0 {
		if len(writeBuf) > maxFragment {
			if err := frame.WriteFrame(&fw, connToNewConn(c), writeBuf[:maxFragment], false, rsv1, c.isClient(), op, maskValue); err != nil {
				return err
			}
			writeBuf = writeBuf[maxFragment:]
			op = Continuation
			continue
		}
		return frame.WriteFrame(&fw, connToNewConn(c), writeBuf, true, rsv1, c.isClient(), op, maskValue)
	}
	return nil
}

// TODO
func (c *Conn) WriteTimeout(op Opcode, data []byte, t time.Duration) (err error) {
	if err = c.setWriteDeadline(time.Now().Add(t)); err != nil {
		return
	}

	defer func() { _ = c.setWriteDeadline(time.Time{}) }()
	return c.WriteMessage(op, data)
}

func (c *Conn) WriteControl(op Opcode, data []byte) (err error) {
	if len(data) > maxControlFrameSize {
		return ErrMaxControlFrameSize
	}
	return c.WriteMessage(op, data)
}

func (c *Conn) WriteCloseTimeout(sc StatusCode, t time.Duration) (err error) {
	buf := sc.toBytes()
	return c.WriteTimeout(opcode.Close, buf, t)
}

// data 不能超过125字节
func (c *Conn) WritePing(data []byte) (err error) {
	return c.WriteControl(Ping, data[:])
}

// data 不能超过125字节
func (c *Conn) WritePong(data []byte) (err error) {
	return c.WriteControl(Pong, data[:])
}

func (c *Conn) Close() error {
	if c == nil {
		return nil
	}

	c.closeWithLock(nil)
	return nil
}
