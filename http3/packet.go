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

package http3

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// QUIC 的包头（RFC 9000 section 17）。
//
// 两种形态，用第一个字节的最高位区分：
//
//	长头（1xxxxxxx）握手阶段用。带版本号、连接 ID，还有包类型
//	  Initial / 0-RTT / Handshake / Retry
//	短头（0xxxxxxx）握手完之后用。只有一个连接 ID，省字节
//
// 为什么分两种：握手阶段要带版本协商的信息（版本号、双方连接 ID），
// 数据阶段这些都不需要了——每次多带十几个字节，在每秒百万包的量级上
// 就是白烧带宽。
const (
	// 长头的包类型（第一个字节的 bit 4-5）
	PacketInitial   = 0x0
	PacketZeroRTT   = 0x1
	PacketHandshake = 0x2
	PacketRetry     = 0x3
)

var (
	ErrBadPacket   = errors.New("http3: malformed packet")
	ErrShortPacket = errors.New("http3: packet truncated")
)

// LongHeader 是一个长头包。
type LongHeader struct {
	Type byte // PacketInitial 等

	// Version 是 QUIC 版本。0 表示版本协商（Version Negotiation）
	Version uint32

	// DestConnID / SrcConnID 是连接 ID（长度写在包里，0..20 字节）
	DestConnID []byte
	SrcConnID  []byte

	// PacketNumber 是包号，占 1..4 字节（长度由第一个字节的低两位决定）
	PacketNumber uint64
	// PacketNumberLen 是包号占了几个字节
	PacketNumberLen int

	// Payload 是包号之后的载荷（加密的部分）
	Payload []byte
}

// ShortHeader 是一个短头包（握手之后的数据包）。
type ShortHeader struct {
	// SpinBit 是第一个字节的 bit 5（延迟测量用，我们不处理）
	SpinBit bool
	// KeyPhase 是第一个字节的 bit 2（密钥轮换）
	KeyPhase bool

	// DestConnID 长度不写在包里——接收方按自己发出去的连接 ID 长度解
	DestConnID []byte

	PacketNumber    uint64
	PacketNumberLen int

	Payload []byte
}

// connIDLen 从字节里读连接 ID 长度。
//
// 长头里连接 ID 长度由长度字节决定；短头里没有长度字节（接收方按自己
// 的连接 ID 长度解）。
func connIDLen(b byte) int { return int(b) }

// ReadLongHeader 解析一个长头包。
//
// out 是接收方期望的"本端连接 ID 长度"（短头用），长头不用。
func ReadLongHeader(b []byte) (*LongHeader, error) {
	if len(b) < 1 {
		return nil, ErrShortPacket
	}
	h := &LongHeader{}
	h.Type = (b[0] >> 4) & 0x3

	// 版本号（4 字节）
	if len(b) < 5 {
		return nil, ErrShortPacket
	}
	h.Version = binary.BigEndian.Uint32(b[1:5])
	off := 5

	// 版本协商包：版本号是 0，后面是"支持的版本列表"，没有连接 ID
	if h.Version == 0 {
		h.Payload = b[5:]
		return h, nil
	}

	// 目的连接 ID：1 字节长度 + 内容
	if off >= len(b) {
		return nil, ErrShortPacket
	}
	dcil := connIDLen(b[off])
	off++
	if dcil > 20 {
		return nil, fmt.Errorf("%w: dest conn id %d > 20", ErrBadPacket, dcil)
	}
	if off+dcil > len(b) {
		return nil, ErrShortPacket
	}
	h.DestConnID = b[off : off+dcil]
	off += dcil

	// 源连接 ID
	if off >= len(b) {
		return nil, ErrShortPacket
	}
	scil := connIDLen(b[off])
	off++
	if scil > 20 {
		return nil, fmt.Errorf("%w: src conn id %d > 20", ErrBadPacket, scil)
	}
	if off+scil > len(b) {
		return nil, ErrShortPacket
	}
	h.SrcConnID = b[off : off+scil]
	off += scil

	// 包号：长度由第一个字节的低两位决定（1..4 字节），**包号本身也要
	// 跳过**——Payload 是它之后的部分。早先漏了这一步，Payload 里就带上
	// 了包号（实测解出来是 "\x00*payload" 而不是 "payload"）。
	//
	// 注意 Initial 包在包号之前还有一个 token 长度字段，那种带 token 的
	// 包这里先不处理（我们不做 0-RTT，也就不需要 token）。
	pnLen := int(b[0]&0x3) + 1
	if off+pnLen > len(b) {
		return nil, ErrShortPacket
	}
	for i := 0; i < pnLen; i++ {
		h.PacketNumber = h.PacketNumber<<8 | uint64(b[off+i])
	}
	h.PacketNumberLen = pnLen
	off += pnLen

	h.Payload = b[off:]
	return h, nil
}

// ReadShortHeader 解析一个短头包。
//
// connIDLen 是接收方期望的连接 ID 长度（自己发出去的那个）。
func ReadShortHeader(b []byte, connIDLen int) (*ShortHeader, error) {
	if len(b) < 1+connIDLen+1 {
		return nil, ErrShortPacket
	}
	h := &ShortHeader{
		SpinBit:  b[0]&0x20 != 0,
		KeyPhase: b[0]&0x04 != 0,
	}
	off := 1
	h.DestConnID = b[off : off+connIDLen]
	off += connIDLen

	// 包号长度由第一个字节的低两位决定
	pnLen := int(b[0]&0x3) + 1
	if off+pnLen > len(b) {
		return nil, ErrShortPacket
	}
	for i := 0; i < pnLen; i++ {
		h.PacketNumber = h.PacketNumber<<8 | uint64(b[off+i])
	}
	h.PacketNumberLen = pnLen
	off += pnLen

	h.Payload = b[off:]
	return h, nil
}

// IsLongHeader 判断一个包是不是长头（第一个字节的最高位）。
func IsLongHeader(b byte) bool { return b&0x80 != 0 }

// AppendLongHeader 拼一个长头。
func AppendLongHeader(dst []byte, ptype byte, version uint32, destConnID, srcConnID []byte, pn uint64, pnLen int) []byte {
	// 第一个字节：1（长头）| 类型 | 保留位 | 包号长度-1
	first := byte(0x80) | (ptype&0x3)<<4 | byte(pnLen-1)
	dst = append(dst, first)

	var v [4]byte
	binary.BigEndian.PutUint32(v[:], version)
	dst = append(dst, v[:]...)

	dst = append(dst, byte(len(destConnID)))
	dst = append(dst, destConnID...)
	dst = append(dst, byte(len(srcConnID)))
	dst = append(dst, srcConnID...)

	// 包号：pnLen 个字节的大端（实际协议里是截断的，这里先按完整写）
	for i := pnLen - 1; i >= 0; i-- {
		dst = append(dst, byte(pn>>(8*uint(i))))
	}
	return dst
}

// AppendShortHeader 拼一个短头。
func AppendShortHeader(dst []byte, connID []byte, pn uint64, pnLen int) []byte {
	first := byte(0x40) | byte(pnLen-1) // 0 = 短头，固定位
	dst = append(dst, first)
	dst = append(dst, connID...)
	for i := pnLen - 1; i >= 0; i-- {
		dst = append(dst, byte(pn>>(8*uint(i))))
	}
	return dst
}
