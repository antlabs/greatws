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
package quicknet

import (
	"sync/atomic"
	"syscall"

	"github.com/antlabs/wsutil/bytespool"
)

// 攒包(cork): 一次 read 里到的多个 frame, 它们的回包合成一次写。
//
// 为什么需要: 一条连接的每条消息一次 write, 而客户端一次写就可能带 10
// 条消息(Pipeline 压测: -rpl 10, 一次 10320 字节)。服务端每条回一条,
// 10 次 write 系统调用, 每次内核都要把 1KB 从用户态搬过去。攒起来就
// 一次 write 搬 10KB, 系统调用数和内核里的每次固定开销都除 10。
//
// fnet 是这么做的(cork/uncork, 见它 websocket/conn.go)。Pipeline 场景
// 里两边的 TPS 都顶在客户端灌入上限, 判定列是 CPU EER, 而我们的写路径
// CPU 是它的大约 3 倍(10 秒 profile: quicknet 43.12s vs fnet 12.81s),
// 差的就是这 9 次多出来的 write。
//
// 做法和 fnet 一致: 解析到一个 frame 后, 如果读缓冲区里还有没解析的
// 字节, 就开攒; 回调里写出去的消息先进 wbufList 那一块缓冲区; 整轮
// read 结束时一次写出去。攒满装不下时先 writev 出去(不拷 payload),
// 再接着攒。
//
// 攒包只发生在 event loop 自己解析(io 模式)、服务端、没压缩的连接上,
// 和 WriteMessage 的快路径条件一致——那些路径本来就是"每条消息一次
// socketWrite"。别的模式(回调在协程池、客户端、压缩)照旧。
//
// 单帧的那种连接(一次 read 一个 frame, 比如 Echo 场景)不会开攒
// (read 完缓冲区里没有下一个 frame), 所以那条路径一点没变。

// maxCorkBytes 是攒包缓冲区的大小上限。
//
// 上限之内取的是池里的内存(bytespool 最大 63KB 左右), 超了 GetBytes
// 会现场 make, 那种内存不进池, 每个批次都新分配一块, 反而多花 GC。
// 拿到的缓冲区还要留 1/4 的余量(回包不一定和请求一样大), 32KB 乘
// 1.25 是 40KB, 离池的上限还有余量。
const maxCorkBytes = 32 * 1024

func (c *Conn) isCorking() bool { return atomic.LoadUint32(&c.packed)&flagCorking != 0 }

func (c *Conn) setCorking(v bool) {
	if v {
		atomic.OrUint32(&c.packed, flagCorking)
	} else {
		atomic.AndUint32(&c.packed, ^flagCorking)
	}
}

// maybeCork 在"这一轮 read 里还有下一个 frame"时开攒。调用方(readPayload
// 之后、回调之前)已经把 rr 推过当前 frame 的 payload, 所以 rw-rr 就是后
// 面还没解析的字节数。
//
// 条件里 c.task == nil 是 io 模式(回调就地执行), 攒包只在那种模式下
// 有意义: 回调在别的 goroutine 上跑时, 写出去的东西和解析的批次对不
// 上号。
func (c *Conn) maybeCork() {
	if c.isCorking() || c.task != nil || c.isClient() {
		return
	}
	// 调用方(读路径)是连接自己的 goroutine; 但 Close 可能从别的
	// goroutine 来, 拿一下锁表示对 wbufList 的所有权, 和 WriteMessage
	// 保持同一套约定。
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.wbufList) != 0 {
		// 有积压: 先让它写完, 攒包和积压抢写事件会乱序。
		return
	}
	rem := c.rw - c.rr
	if rem <= 0 {
		// 当前 frame 后面没有别的数据了, 这一批只有它一个。
		return
	}
	c.corkStartLocked(rem)
}

// corkStartLocked 取一块攒包缓冲区。调用方持有 c.mu。
func (c *Conn) corkStartLocked(capHint int) {
	if capHint > maxCorkBytes {
		capHint = maxCorkBytes
	}
	// 回包总长和请求总长差不多(echo), 留 1/4 余量, 这样一批回包通常
	// 一个缓冲区就装下, 不用中途 writev 一次。
	capHint += capHint / 4

	b := bytespool.GetBytes(capHint)
	*b = (*b)[:0]
	c.wbufList = append(c.wbufList[:0], b)
	c.setCorking(true)
}

// corkWrite 把一条消息写进攒包缓冲区, 装不下就先把攒的这批和新消息一次
// writev 出去。
//
// 调用方持有 c.mu(WriteMessage 的快路径都在锁里)。
func (c *Conn) corkWrite(op uint8, payload []byte) error {
	if len(c.wbufList) == 0 {
		// 攒包缓冲区被写事件那条路消费掉了(部分写之后 flush 成功),
		// 重新取一块接着攒。
		c.corkStartLocked(len(payload))
	}
	b := c.wbufList[len(c.wbufList)-1]

	var hdr [10]byte
	hn := wsHeader(hdr[:], op, len(payload))
	if len(*b)+hn+len(payload) <= cap(*b) {
		*b = append(*b, hdr[:hn]...)
		*b = append(*b, payload...)
		return nil
	}

	// 装不下: 先换一块大的再来。跳一次到位(见 corkGrowBytes), 只多拷
	// 一次"已经攒下的这些"; 不然就会退回"每两条一次 writev", 攒包就没
	// 意义了。开攒时那个大小提示只是一次 read 里剩下的字节数, 开头总是
	// 偏小, 靠这一步补上。
	if nb := c.growCork(b, len(*b)+hn+len(payload)); nb != nil {
		b = nb
		*b = append(*b, hdr[:hn]...)
		*b = append(*b, payload...)
		return nil
	}

	// 装不下也换不动(已经很大了, 或者这批比上限还长): 攒的这批和新
	// frame 一次发出去。payload 不拷, 直接进 iovec, 和 fnet 的
	// writeCorked 一样。
	seg := *b
	n, err := socketWritev3(c.getFd(), seg, hdr[:hn], payload)
	c.addWriteSyscall()
	if n < 0 {
		// 兜底: 下面要拿 n 切 slices, syscall 层返回负数的话这里就成了
		// "从一个负下标切", 直接 panic。socketWritev3 保证出错时返回 0,
		// 但这条路径不该因为一个返回值约定被破坏就崩掉整个进程。
		n = 0
	}
	total := len(seg) + hn + len(payload)
	if err == nil && n == total {
		*b = (*b)[:0]
		return nil
	}
	if err == nil || err == syscall.EAGAIN || err == syscall.EINTR {
		// 部分写: 没写出去的那部分留在 wbufList 里, 剩下的交给可写事件。
		// 攒包到此为止(这个批次剩下的消息走普通写路径), 不然它们会插到
		// 这批没写完的数据前面去。
		c.setCorking(false)
		*b = (*b)[:0]
		switch {
		case n < len(seg):
			c.appendToWbufList(seg[n:], total-n)
			c.appendToWbufList(hdr[:hn], hn+len(payload))
			c.appendToWbufList(payload, len(payload))
		case n < len(seg)+hn:
			c.appendToWbufList(hdr[n-len(seg):hn], hn+len(payload)-n+len(seg))
			c.appendToWbufList(payload, len(payload))
		default:
			c.appendToWbufList(payload[n-len(seg)-hn:], total-n)
		}
		if werr := c.eventLoop().addWrite(c); werr != nil {
			return werr
		}
		return nil
	}
	*b = (*b)[:0]
	return err
}

// corkGrowBytes 是"装不下时换多大": 一次换到位, 之后这个批次基本不会再
// 换。选 16KB 是因为 Pipeline 的批次是 -rpl × 消息大小(压测里 10 × 1032
// ≈ 10KB), 而 bytespool 按 1KB 分档, 16KB 的请求拿到的是 15KB 那档,
// 装得下 10KB 的批次还留了余量。
//
// 比它更大的批次(比如 -rbs 64KB)装不下时会走 writev 那条路, 每 15KB
// 一次写——也比每条一次好得多。
const corkGrowBytes = 16 * 1024

// growCork 把攒包缓冲区换成一块够大的, 换不动(已经不小于 corkGrowBytes,
// 或者要装的东西比它还长)时返回 nil。
func (c *Conn) growCork(old *[]byte, need int) *[]byte {
	if cap(*old) >= corkGrowBytes || need > corkGrowBytes {
		return nil
	}
	nb := bytespool.GetBytes(corkGrowBytes)
	if cap(*nb) < need {
		// 池里这一档不够(它的档位是按请求大小分桶的), 不折腾了。
		bytespool.PutBytes(nb)
		return nil
	}
	*nb = append((*nb)[:0], (*old)...)
	bytespool.PutBytes(old)
	c.wbufList[len(c.wbufList)-1] = nb
	return nb
}

// corkEnd 结束攒包, 把攒下的回包一次写出去。解析那一轮(read 循环)的
// 收尾调用, 失败路径也要调: 攒下的回包要在 close 帧之前出去。
func (c *Conn) corkEnd() {
	if !c.isCorking() {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// 清位在锁里: 别的 goroutine 的 WriteMessage 拿着锁读这个位, 读到的
	// 要么是"还在攒"(它写进缓冲区, 由这里一起写出去), 要么是"攒完了"
	// (它走普通写路径)。锁外清位的话, 它可能读到过期的"还在攒", 把数据
	// 塞进一个不会再有人来写的缓冲区。
	c.setCorking(false)
	if len(c.wbufList) == 0 {
		return
	}
	// 攒包缓冲区可能是空的: 整批被 writev 溢出那条路写出去之后缓冲区就清
	// 空了(见 corkWrite)。空的还回去, 不要走 flush——它会为 0 字节也记一
	// 次写系统调用。
	if len(c.wbufList) == 1 && len(*c.wbufList[0]) == 0 {
		bytespool.PutBytes(c.wbufList[0])
		c.wbufList[0] = nil
		c.wbufList = c.wbufList[:0]
		return
	}
	// flush 自己负责把写完的缓冲区还回池子(write 里那套: 还一块、
	// wbufList[i] = nil、最后 wbufList = wbufList[:0])。别在这里再动
	// wbufList——flush 可能是提前返回的(连接已关), 那时列表还在, 逐个
	// 置 nil 会留下一个"非空但全是 nil"的列表, 下一个 appendToWbufList
	// 就会解引用 nil。
	c.flush()
}
