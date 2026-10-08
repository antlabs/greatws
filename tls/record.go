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

package tls

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// TLS 1.3 的记录层（RFC 8446 section 5）。
//
// 这是**状态机实现的第一层**：把字节流切成记录（TLSRecord），每条记录带
// 自己的类型和长度。握手消息、告警、应用数据都装在记录里。
//
//	TCP 字节流
//	  └── TLS 记录（5 字节头 + 明文/密文）
//	        └── 握手消息 / 应用数据 / 告警
//
// 为什么状态机而不是阻塞调用：这一层只要"喂字节、吐记录"，数据不够就
// 返回"还要"，套在非阻塞 fd 上时不会卡住事件循环（见 memconn.go 里
// 那段说明——crypto/tls 做不到这件事，所以才有这个包）。
const (
	// 记录类型（RFC 8446 section 5.1）
	recordChangeCipherSpec uint8 = 20
	recordAlert            uint8 = 21
	recordHandshake        uint8 = 22
	recordApplicationData  uint8 = 23
)

// recordHeaderLen 是记录头的长度：1 类型 + 2 版本 + 2 长度。
const recordHeaderLen = 5

// maxRecordPayload 是一条记录里明文的上限（RFC 8446 5.1: 2^14）。
const maxRecordPayload = 1 << 14

// maxCiphertextLen 是密文的上限：明文上限 + 256（多出来的是 AEAD 的
// 认证标签和内层类型）。
const maxCiphertextLen = maxRecordPayload + 256

var (
	ErrRecordTooLarge = errors.New("tls: record exceeds max size")
	ErrBadRecord      = errors.New("tls: malformed record")
)

// Record 是一条 TLS 记录。
//
// payload 指向调用方的缓冲区（不拷贝），只在这次处理里有效。
type Record struct {
	Type    uint8
	Version uint16 // 0x0303 = TLS 1.2（1.3 在记录层也写这个）
	Payload []byte
}

// RecordParser 从字节流里切记录。
//
// 用法和非阻塞 io 对得上：喂一段字节，切出完整的记录；不够一条的留着，
// 下次再喂。**不会阻塞，也不会自己攒**——攒的活交给调用方（它本来就有
// 读缓冲区）。
type RecordParser struct {
	// buf 攒着还没凑成完整记录的字节
	buf []byte
}

// NewRecordParser 建一个记录解析器。
func NewRecordParser() *RecordParser { return &RecordParser{} }

// Reset 清空。
func (p *RecordParser) Reset() { p.buf = p.buf[:0] }

// Buffered 还有多少字节攒着。
func (p *RecordParser) Buffered() int { return len(p.buf) }

// Parse 喂一段字节，每切出一条完整记录就调 fn。
//
// 返回消化了多少（按这次传进来的 data 算）。攒在内部的那部分也算消化
// ——不然调用方会重复喂。
func (p *RecordParser) Parse(data []byte, fn func(*Record) error) (int, error) {
	// **不攒**：不够一条记录的字节原样留给调用方。
	//
	// 为什么：调用方（engine 的读循环）本来就有一块读缓冲区，它按"协议
	// 返回多少就丢掉多少、剩下的留着"来工作。**如果这里也攒一份，就
	// 重复了**——同一个字节既在状态机的 buf 里、又在引擎的缓冲区里。
	//
	// 实测的坑：喂 8 字节（不够一条记录）返回 0，引擎留着那 8 字节；
	// 状态机也攒了。下次喂剩 8 字节，状态机拼出 16 字节解出记录、返回
	// 8——引擎以为"消化了 8"，把第二次那 8 字节丢掉。**第一次那 8 字节
	// 被处理了两次，净效果是账算不平**（两次消化之和 8 ≠ 总长 16）。
	//
	// 症状：客户端把 Finished 和应用数据放在同一个 TCP 段里发过来时，
	// 应用数据那条记录就是解不出来（内层 handler 一次都没被调到）。
	//
	// 代价：跨 Feed 的半个记录要由调用方保管。engine 的读缓冲区正好
	// 干这个。
	work := data
	consumed := 0
	var err error
	for {
		if len(work)-consumed < recordHeaderLen {
			break // 头还没齐
		}
		head := work[consumed : consumed+recordHeaderLen]
		length := int(binary.BigEndian.Uint16(head[3:5]))

		if length > maxCiphertextLen {
			err = fmt.Errorf("%w: %d > %d", ErrRecordTooLarge, length, maxCiphertextLen)
			break
		}
		if len(work)-consumed-recordHeaderLen < length {
			break // 载荷还没齐
		}

		r := &Record{
			Type:    head[0],
			Version: binary.BigEndian.Uint16(head[1:3]),
			Payload: work[consumed+recordHeaderLen : consumed+recordHeaderLen+length],
		}
		if cbErr := fn(r); cbErr != nil {
			err = cbErr
			break
		}
		consumed += recordHeaderLen + length
	}

	// 没成记录的字节留给调用方（这里不攒，见 Parse 开头的说明）
	return consumed, err
}

// AppendRecord 拼一条记录（5 字节头 + 载荷）。
func AppendRecord(dst []byte, rtype uint8, version uint16, payload []byte) []byte {
	dst = append(dst, rtype,
		byte(version>>8), byte(version),
		byte(len(payload)>>8), byte(len(payload)))
	return append(dst, payload...)
}

// ---------------------------------------------------------------------------
// 握手消息（RFC 8446 section 4）

// 握手消息类型
const (
	hsClientHello         uint8 = 1
	hsServerHello         uint8 = 2
	hsNewSessionTicket    uint8 = 4
	hsEncryptedExtensions uint8 = 8
	hsCertificate         uint8 = 11
	hsCertificateVerify   uint8 = 15
	hsFinished            uint8 = 20
	hsKeyUpdate           uint8 = 24
)

// 握手头：1 字节类型 + 3 字节长度。
const handshakeHeaderLen = 4

var ErrBadHandshake = errors.New("tls: malformed handshake message")

// Handshake 是一条握手消息。
type Handshake struct {
	Type    uint8
	Payload []byte
}

// HandshakeParser 从握手记录的内容里切握手消息。
//
// **一层比一层细**：记录层给出"这一段是握手"，这一层再把它切成一条条
// 握手消息（一个记录里可能有好几条消息——比如 ServerHello 之后紧跟
// ChangeCipherSpec 之前的那批）。
type HandshakeParser struct {
	buf []byte
}

func NewHandshakeParser() *HandshakeParser { return &HandshakeParser{} }

func (p *HandshakeParser) Reset() { p.buf = p.buf[:0] }

func (p *HandshakeParser) Buffered() int { return len(p.buf) }

// Parse 喂一段字节（记录里的握手数据），切出握手消息。
func (p *HandshakeParser) Parse(data []byte, fn func(*Handshake) error) (int, error) {
	// **这里要攒**（和 RecordParser 不同）：一条握手消息可以跨多条 TLS
	// 记录（比如证书链），而调用方是一条记录一条记录喂进来的。所以半条
	// 消息必须留在内部等下一段。
	//
	// 消化量按"这次的 data"算——攒着的那部分调用方已经交出来了（它是
	// 上一条记录的内容），不能再算一次。
	oldLen := len(p.buf)
	work := data
	if oldLen > 0 {
		p.buf = append(p.buf, data...)
		work = p.buf
	}

	consumed := 0
	var err error
	for {
		if len(work)-consumed < handshakeHeaderLen {
			break
		}
		typ := work[consumed]
		length := int(work[consumed+1])<<16 | int(work[consumed+2])<<8 | int(work[consumed+3])

		if length > 1<<20 {
			err = fmt.Errorf("%w: handshake message %d bytes", ErrBadHandshake, length)
			break
		}
		if len(work)-consumed-handshakeHeaderLen < length {
			break
		}

		h := &Handshake{
			Type:    typ,
			Payload: work[consumed+handshakeHeaderLen : consumed+handshakeHeaderLen+length],
		}
		if cbErr := fn(h); cbErr != nil {
			err = cbErr
			break
		}
		consumed += handshakeHeaderLen + length
	}

	// 剩下没凑成完整消息的留在 p.buf 里（下次接着拼）
	if rest := work[consumed:]; len(rest) > 0 {
		p.buf = append(p.buf[:0], rest...)
	} else {
		p.buf = p.buf[:0]
	}
	// 消化量按"这次的 data"算：work 前 oldLen 字节是上一轮攒的
	n := consumed - oldLen
	if n < 0 {
		n = 0
	}
	if n > len(data) {
		n = len(data)
	}
	return n, err
}

// AppendHandshake 拼一条握手消息（4 字节头 + 载荷）。
func AppendHandshake(dst []byte, typ uint8, payload []byte) []byte {
	dst = append(dst, typ,
		byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload)))
	return append(dst, payload...)
}

func handshakeTypeName(t uint8) string {
	switch t {
	case hsClientHello:
		return "ClientHello"
	case hsServerHello:
		return "ServerHello"
	case hsNewSessionTicket:
		return "NewSessionTicket"
	case hsEncryptedExtensions:
		return "EncryptedExtensions"
	case hsCertificate:
		return "Certificate"
	case hsCertificateVerify:
		return "CertificateVerify"
	case hsFinished:
		return "Finished"
	case hsKeyUpdate:
		return "KeyUpdate"
	}
	return fmt.Sprintf("Unknown(%d)", t)
}
