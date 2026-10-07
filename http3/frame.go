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
	"errors"
	"fmt"
)

// HTTP/3 的帧（RFC 9114 section 7）。
//
// **它在两个层次之下**：QUIC 包 -> QUIC 流 -> HTTP/3 帧。
//
//	UDP 数据报
//	  └── QUIC 包（带连接 ID、包号，加密）
//	        └── QUIC 流（可靠、有序的字节流，一个连接里有很多条）
//	              └── HTTP/3 帧（这一层才是请求/响应）
//
// 所以 HTTP/3 的"帧"和 HTTP/2 的帧完全是两回事：HTTP/2 的帧直接躺在
// TCP 字节流上，边界靠帧头里的长度；HTTP/3 的帧躺在 QUIC 流上，流保证
// 了有序和可靠，帧只需要自己声明长度。
//
// 类型和长度都是 QUIC 的变长整数（见 varint.go）。
type FrameType uint64

const (
	// FrameData 是消息体
	FrameData FrameType = 0x0
	// FrameHeaders 是请求/响应的头（QPACK 编码）
	FrameHeaders FrameType = 0x1
	// FrameCancelPush 取消服务端推送
	FrameCancelPush FrameType = 0x3
	// FrameSettings 是连接级的设置（在控制流上）
	FrameSettings FrameType = 0x4
	// FramePushPromise 服务端推送的预告
	FramePushPromise FrameType = 0x5
	// FrameGoAway 关连接
	FrameGoAway FrameType = 0x7
	// FrameMaxPushID 推送 ID 上限
	FrameMaxPushID FrameType = 0xd
)

// 在哪个流上发：HTTP/3 的流分三种。
const (
	// StreamControl 是控制流（每个方向一条），放 SETTINGS、GOAWAY
	StreamControl uint64 = 0x00
	// StreamPush 是服务端推送流
	StreamPush uint64 = 0x01
	// StreamQPACKEncoder 是 QPACK 编码器的专用流
	StreamQPACKEncoder uint64 = 0x02
	// StreamQPACKDecoder 是 QPACK 解码器的专用流
	StreamQPACKDecoder uint64 = 0x03
)

// 请求流（用 QUIC 的 StreamID 规则：低位是类型，再上面一位是发起方）。
//
// QUIC 的流 ID 编码：最低两位是流类型（0 客户端发起的双向流、
// 1 服务端发起的双向流、2 客户端发起的单向、3 服务端发起的单向），
// 再往上一位是"这个类型的第几条流"。
//
// **HTTP/3 的请求走双向流**：客户端发起一条，发请求、收响应。所以
// 请求流是客户端发起的双向流（低位 00）。
func IsRequestStream(id uint64) bool { return id&0x3 == 0 }

var (
	ErrBadFrame     = errors.New("http3: malformed frame")
	ErrFrameTooLong = errors.New("http3: frame payload too large")
)

// Frame 是一个 HTTP/3 帧。
type Frame struct {
	Type    FrameType
	Payload []byte
}

func (t FrameType) String() string {
	switch t {
	case FrameData:
		return "DATA"
	case FrameHeaders:
		return "HEADERS"
	case FrameCancelPush:
		return "CANCEL_PUSH"
	case FrameSettings:
		return "SETTINGS"
	case FramePushPromise:
		return "PUSH_PROMISE"
	case FrameGoAway:
		return "GOAWAY"
	case FrameMaxPushID:
		return "MAX_PUSH_ID"
	}
	return fmt.Sprintf("UNKNOWN(0x%x)", uint64(t))
}

// maxFramePayload 是单帧载荷的上限。
//
// 和 HTTP/2 一样是"防护"：长度字段是变长整数，对端可以声明一个天文数字，
// 我们不检查就会一直等（或者分配一块巨大的内存）。
const maxFramePayload = 16 * 1024 * 1024

// FrameParser 从一条 QUIC 流的字节流里切 HTTP/3 帧。
//
// 用法和 http2 的帧解析器一样：喂字节，切出完整的帧；不够的留着。
// 区别是这里长度用变长整数（QUIC 的），不是定长 3 字节。
type FrameParser struct {
	// buf 攒着还没凑成完整帧的字节
	buf []byte
}

// NewFrameParser 建一个帧解析器（一条流一个）。
func NewFrameParser() *FrameParser { return &FrameParser{} }

// Reset 清空（流结束、复用解析器）。
func (p *FrameParser) Reset() { p.buf = p.buf[:0] }

// Buffered 还有多少字节攒着。
func (p *FrameParser) Buffered() int { return len(p.buf) }

// Parse 喂一段字节，每切出一个完整帧就调 fn。
//
// 返回消化了多少（按这次传进来的 data 算）。和 grpc 的消息解析器一样，
// **攒在内部的那部分也算消化**——不然调用方会重复喂。
func (p *FrameParser) Parse(data []byte, fn func(*Frame) error) (int, error) {
	work := data
	if len(p.buf) > 0 {
		p.buf = append(p.buf, data...)
		work = p.buf
	}

	consumed := 0
	var err error
	for {
		// 类型（变长整数）
		ftype, n, e := ReadVarint(work[consumed:])
		if e != nil {
			break // 类型都没读全
		}
		// 长度（变长整数）
		length, m, e := ReadVarint(work[consumed+n:])
		if e != nil {
			break
		}
		if length > maxFramePayload {
			err = fmt.Errorf("%w: %d > %d", ErrFrameTooLong, length, maxFramePayload)
			break
		}
		headLen := n + m
		if uint64(len(work)-consumed-headLen) < length {
			break // 载荷还没到齐
		}

		f := &Frame{
			Type:    FrameType(ftype),
			Payload: work[consumed+headLen : consumed+headLen+int(length)],
		}
		if cbErr := fn(f); cbErr != nil {
			err = cbErr
			break
		}
		consumed += headLen + int(length)
	}

	if rest := work[consumed:]; len(rest) > 0 {
		p.buf = append(p.buf[:0], rest...)
	} else {
		p.buf = p.buf[:0]
	}
	// 消化量按"这次的 data"算：攒在 p.buf 里的那部分也算收下了——
	// 不然调用方会重复喂（和 grpc 的消息解析器同一个道理）。
	return len(data), err
}

// AppendFrame 拼一个 HTTP/3 帧（类型 + 长度 + 载荷）。
func AppendFrame(dst []byte, ftype FrameType, payload []byte) []byte {
	dst = AppendVarint(dst, uint64(ftype))
	dst = AppendVarint(dst, uint64(len(payload)))
	return append(dst, payload...)
}

// AppendData 拼一个 DATA 帧。
func AppendData(dst []byte, body []byte) []byte {
	return AppendFrame(dst, FrameData, body)
}

// AppendHeaders 拼一个 HEADERS 帧（payload 是 QPACK 编码过的头块）。
func AppendHeaders(dst []byte, block []byte) []byte {
	return AppendFrame(dst, FrameHeaders, block)
}

// ---------------------------------------------------------------------------
// SETTINGS（控制流上的那个）

// SETTINGS 的标识（RFC 9114 section 7.2.8）。
const (
	SettingQPACKMaxTableCapacity uint64 = 0x01
	SettingMaxFieldSectionSize   uint64 = 0x06
	SettingQPACKBlockedStreams   uint64 = 0x07
	SettingEnableConnectProtocol uint64 = 0x08
	SettingH3Datagram            uint64 = 0x33
)

// Setting 是一项设置。
type Setting struct {
	ID    uint64
	Value uint64
}

// AppendSettings 拼一个 SETTINGS 帧。
func AppendSettings(dst []byte, settings ...Setting) []byte {
	// 先算载荷长度（每项是"ID 变长 + 值变长"）
	var payload []byte
	for _, s := range settings {
		payload = AppendVarint(payload, s.ID)
		payload = AppendVarint(payload, s.Value)
	}
	return AppendFrame(dst, FrameSettings, payload)
}

// ParseSettings 解一个 SETTINGS 帧的载荷。
//
// 返回的切片指向 payload，不拷贝。
func ParseSettings(payload []byte) ([]Setting, error) {
	var out []Setting
	i := 0
	for i < len(payload) {
		id, n, err := ReadVarint(payload[i:])
		if err != nil {
			return nil, fmt.Errorf("%w: setting id: %v", ErrBadFrame, err)
		}
		val, m, err := ReadVarint(payload[i+n:])
		if err != nil {
			return nil, fmt.Errorf("%w: setting value: %v", ErrBadFrame, err)
		}
		out = append(out, Setting{ID: id, Value: val})
		i += n + m
	}
	return out, nil
}
