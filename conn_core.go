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
	wbuf                 *[]byte            // 写缓冲区, 当直接Write失败时，会将数据写入缓冲区
	lenAndMaskSize       int                // payload长度和掩码的长度
	rh                   frame.FrameHeader  // frame头部
	fragmentFramePayload *[]byte            // 存放分片帧的缓冲区
	fragmentFrameHeader  *frame.FrameHeader // 存放分段帧的头部
	lastPayloadLen       int32              // 上一次读取的payload长度, TODO启用
	curState             frameState         // 保存当前状态机的状态
	client               bool               // 客户端为true，服务端为false
}

func (c *Conn) getLogger() *slog.Logger {
	return c.multiEventLoop.Logger
}

func (c *Conn) addTask(f func() bool) {
	if c.isClosed() {
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
	state := c.curState
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
		c.curState, state = frameStateHeaderPayloadAndMask, frameStateHeaderPayloadAndMask
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
		c.curState = frameStatePayload
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

func (c *Conn) writeCap() int {
	return len((*c.rbuf)[c.rw:])
}

// 需要考虑几种情况
// 返回完整Payload逻辑
// 1. 当前的rbuf长度不够，需要重新分配
// 2. 当前的rbuf长度够，但是数据没有读完整
// 返回分片Paylod逻辑
// TODO
func (c *Conn) readPayload() (f frame.Frame2, success bool, err error) {
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
	c.lastPayloadLen = int32(c.rh.PayloadLen)
	// 普通frame
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

func (c *Conn) processCallback(f frame.Frame2) (err error) {
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

			if c.fragmentFramePayload == nil {
				c.fragmentFramePayload = f.Payload
			} else {
				*c.fragmentFramePayload = append(*c.fragmentFramePayload, *f.Payload...)
				bytespool.PutBytes(f.Payload)
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
				// greatws和quickws，这时的f.Payload是单独分配出来的，所以转移下变量的所有权就行
				c.fragmentFramePayload = f.Payload
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

		// text或者binary进入业务协程执行
		c.addTask(func() bool {
			return c.processCallbackData(f, payload, rsv1, decompression, needMask, maskKey)
		})

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

// 如果是text或者binary的消息， 在这里调用OnMessage函数
func (c *Conn) processCallbackData(f frame.Frame2, payload *[]byte, rsv1 bool, decompression bool, needMask bool, maskKey uint32) (ok bool) {
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
			bytespool.PutBytes(payload)
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
	bytespool.PutBytes(payload)
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
	if c.curState == frameStatePayload {
		f, success, err := c.readPayload()
		if err != nil {
			c.getLogger().Error("readPayloadAndCallback.read payload err", "err", err.Error())
			return sucess, err
		}

		// fmt.Printf("read payload, success:%t, %v\n", success, f.Payload)
		if success {
			if err := c.processCallback(f); err != nil {
				c.closeWithLock(err)
				return false, err
			}
			c.curState = frameStateHeaderStart
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
	if c.client {
		maskValue = rand.Uint32()
	}

	var fw fixedwriter.FixedWriter

	c.mu.Lock()
	err = frame.WriteFrame(&fw, c, writeBuf, true, rsv1, c.client, op, maskValue)
	c.mu.Unlock()

	return err
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
	if c.client {
		maskValue = rand.Uint32()
	}

	var fw fixedwriter.FixedWriter
	for len(writeBuf) > 0 {
		if len(writeBuf) > maxFragment {
			if err := frame.WriteFrame(&fw, c, writeBuf[:maxFragment], false, rsv1, c.client, op, maskValue); err != nil {
				return err
			}
			writeBuf = writeBuf[maxFragment:]
			op = Continuation
			continue
		}
		return frame.WriteFrame(&fw, c, writeBuf, true, rsv1, c.client, op, maskValue)
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

func (c *Conn) Close() {
	c.closeWithLock(nil)
}
