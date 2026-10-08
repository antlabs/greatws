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
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"fmt"
)

// TLS 1.3 的握手状态机（RFC 8446 section 4）。
//
// **这是"基于状态机"的那一层**——不用 crypto/tls，自己做。为什么不能
// 用：crypto/tls 的握手不是分步的（它内部的 handshakeErr 一旦置上，
// 后面每次调用都直接返回那个错误），套在非阻塞 fd 上就只能把整个事件
// 循环卡住。
//
// 这里的状态机长这样：
//
//	服务端                              客户端
//	StateStart                          StateStart
//	  ↓ 收到 ClientHello                   ↓ 发 ClientHello
//	StateWaitClientHello -> 发 ServerHello + EE + Cert + CV + Fin
//	  ↓                                    ↓ 收到（逐个处理）
//	StateWaitFinished                    StateWaitServerFinished -> 发 Fin
//	  ↓ 收到 Finished                      ↓
//	StateEstablished                     StateEstablished
//
// 每一步都是"喂我一段字节，我告诉你我还缺什么、要发什么出去"——**不阻塞**。
type HandshakeState uint8

const (
	// StateStart 还没开始
	StateStart HandshakeState = iota
	// StateWaitClientHello 服务端等客户端的 ClientHello
	StateWaitClientHello
	// StateWaitServerHello 客户端等服务端的 ServerHello
	StateWaitServerHello
	// StateWaitEncryptedExtensions 客户端等在 ServerHello 之后的加密扩展
	StateWaitEncryptedExtensions
	// StateWaitCertificate 客户端等证书
	StateWaitCertificate
	// StateWaitCertificateVerify 客户端等证书校验
	StateWaitCertificateVerify
	// StateWaitServerFinished 客户端等服务端 Finished
	StateWaitServerFinished
	// StateWaitFinished 服务端等客户端 Finished
	StateWaitFinished
	// StateEstablished 握手完了
	StateEstablished
	// StateFailed 握手失败了
	StateFailed
)

func (s HandshakeState) String() string {
	switch s {
	case StateStart:
		return "Start"
	case StateWaitClientHello:
		return "WaitClientHello"
	case StateWaitServerHello:
		return "WaitServerHello"
	case StateWaitEncryptedExtensions:
		return "WaitEncryptedExtensions"
	case StateWaitCertificate:
		return "StateWaitCertificate"
	case StateWaitCertificateVerify:
		return "StateWaitCertificateVerify"
	case StateWaitServerFinished:
		return "WaitServerFinished"
	case StateWaitFinished:
		return "WaitFinished"
	case StateEstablished:
		return "Established"
	case StateFailed:
		return "Failed"
	}
	return fmt.Sprintf("State(%d)", uint8(s))
}

// ClientHello 是客户端的第一条消息（RFC 8446 section 4.1.2）。
//
// 字段裁剪到握手必需的几个：版本、随机数、会话 ID、密码套件、扩展。
type ClientHello struct {
	// LegacyVersion 在 TLS 1.3 里固定写 0x0303（1.2），真正的版本在
	// supported_versions 扩展里。这是为了兼容只认 1.2 的中间设备。
	LegacyVersion uint16
	Random        [32]byte
	// SessionID 是兼容用的（1.3 用它做 PSK 的载体），可以是空的
	SessionID []byte
	// CipherSuites 是客户端支持的套件
	CipherSuites []uint16
	// ServerName 是 SNI（从 server_name 扩展里来）
	ServerName string
}

// ServerHello 是服务端的回应。
type ServerHello struct {
	LegacyVersion uint16
	Random        [32]byte
	SessionID     []byte
	// CipherSuite 是服务端选的套件
	CipherSuite uint16
}

// 密码套件（只支持最常用的那几个）。
const (
	// TLS_AES_128_GCM_SHA256 是最常用也是必被支持的
	TLS_AES_128_GCM_SHA256 uint16 = 0x1301
	// TLS_AES_256_GCM_SHA384 用 SHA384，密钥计划那套参数不同
	TLS_AES_256_GCM_SHA384 uint16 = 0x1302
	// TLS_CHACHA20_POLY1305_SHA256 没有 AES 硬件加速时更快
	TLS_CHACHA20_POLY1305_SHA256 uint16 = 0x1303
)

var (
	ErrNoSharedCipher     = errors.New("tls: no shared cipher suite")
	ErrBadClientHello     = errors.New("tls: malformed ClientHello")
	ErrBadServerHello     = errors.New("tls: malformed ServerHello")
	ErrUnsupportedVersion = errors.New("tls: only TLS 1.3 is supported")
)

// ---------------------------------------------------------------------------
// ClientHello 的解析和生成

// ParseClientHello 解析一个 ClientHello。
func ParseClientHello(b []byte) (*ClientHello, error) {
	// legacy_version(2) + random(32)
	if len(b) < 34 {
		return nil, ErrBadClientHello
	}
	ch := &ClientHello{
		LegacyVersion: uint16(b[0])<<8 | uint16(b[1]),
	}
	copy(ch.Random[:], b[2:34])

	off := 34
	// session_id: 1 字节长度 + 内容
	if off >= len(b) {
		return nil, ErrBadClientHello
	}
	sidLen := int(b[off])
	off++
	if off+sidLen > len(b) {
		return nil, ErrBadClientHello
	}
	ch.SessionID = b[off : off+sidLen]
	off += sidLen

	// cipher_suites: 2 字节长度 + 每项 2 字节
	if off+2 > len(b) {
		return nil, ErrBadClientHello
	}
	csLen := int(b[off])<<8 | int(b[off+1])
	off += 2
	if csLen%2 != 0 || off+csLen > len(b) {
		return nil, ErrBadClientHello
	}
	for i := 0; i < csLen; i += 2 {
		ch.CipherSuites = append(ch.CipherSuites, uint16(b[off+i])<<8|uint16(b[off+i+1]))
	}
	off += csLen

	// compression_methods: 1 字节长度 + 内容（TLS 1.3 里必须是 [0]）
	if off >= len(b) {
		return nil, ErrBadClientHello
	}
	cmLen := int(b[off])
	off++
	if off+cmLen > len(b) {
		return nil, ErrBadClientHello
	}
	off += cmLen

	// extensions: 2 字节长度 + 内容
	if off+2 > len(b) {
		// 没有扩展也是合法的（老客户端），但 TLS 1.3 必须有
		// supported_versions
		return ch, nil
	}
	extLen := int(b[off])<<8 | int(b[off+1])
	off += 2
	if off+extLen > len(b) {
		return nil, ErrBadClientHello
	}
	parseExtensions(b[off:off+extLen], func(typ uint16, data []byte) {
		switch typ {
		case extServerName:
			// server_name: 2 字节列表长度 + (1 字节类型 + 2 字节长度 + 名字)
			if len(data) < 5 {
				return
			}
			nameLen := int(data[3])<<8 | int(data[4])
			if 5+nameLen <= len(data) {
				ch.ServerName = string(data[5 : 5+nameLen])
			}
		}
	})
	return ch, nil
}

// AppendClientHello 拼一个 ClientHello。
func AppendClientHello(dst []byte, ch *ClientHello, keyShare []byte) []byte {
	// 消息体
	body := make([]byte, 0, 256)
	body = append(body, byte(ch.LegacyVersion>>8), byte(ch.LegacyVersion))
	body = append(body, ch.Random[:]...)

	body = append(body, byte(len(ch.SessionID)))
	body = append(body, ch.SessionID...)

	// cipher_suites
	body = append(body, byte(len(ch.CipherSuites)*2>>8), byte(len(ch.CipherSuites)*2))
	for _, cs := range ch.CipherSuites {
		body = append(body, byte(cs>>8), byte(cs))
	}

	// compression_methods：TLS 1.3 里必须是 [0]（就是"不压缩"）
	body = append(body, 1, 0)

	// extensions
	var exts []byte
	// supported_versions：只报 1.3
	exts = appendExtension(exts, extSupportedVersions, []byte{2, 0x03, 0x04})
	// supported_groups：声明支持的命名组。**TLS 1.3 里只要带了
	// key_share 就必须带这个**（RFC 8446 4.2.7）。
	//
	// 踩过的坑：早先没发这个扩展，标准库的服务端直接回
	// fatal handshake_failure(40)，而且理由很具体——
	// "no key exchanges supported by both client and server"：它要先从
	// supported_groups 里挑一个组，再拿 key_share 里对应的公钥；没有
	// supported_groups 就没得挑。
	//
	// （自己写的状态机互相握手发现不了这个——两边都不看这个扩展。）
	exts = appendExtension(exts, extSupportedGroups, appendSupportedGroups())
	// key_share：带上 ECDHE 的公钥
	exts = appendExtension(exts, extKeyShare, appendKeyShare(keyShare))
	// signature_algorithms：必需的扩展
	exts = appendExtension(exts, extSignatureAlgorithms, appendSignatureAlgorithms())
	if ch.ServerName != "" {
		exts = appendExtension(exts, extServerName, appendServerName(ch.ServerName))
	}

	body = append(body, byte(len(exts)>>8), byte(len(exts)))
	body = append(body, exts...)

	return AppendHandshake(dst, hsClientHello, body)
}

// ParseServerHello 解析 ServerHello。
func ParseServerHello(b []byte) (*ServerHello, error) {
	if len(b) < 34 {
		return nil, ErrBadServerHello
	}
	sh := &ServerHello{
		LegacyVersion: uint16(b[0])<<8 | uint16(b[1]),
	}
	copy(sh.Random[:], b[2:34])

	off := 34
	if off >= len(b) {
		return nil, ErrBadServerHello
	}
	sidLen := int(b[off])
	off++
	if off+sidLen > len(b) {
		return nil, ErrBadServerHello
	}
	sh.SessionID = b[off : off+sidLen]
	off += sidLen

	if off+2 > len(b) {
		return nil, ErrBadServerHello
	}
	sh.CipherSuite = uint16(b[off])<<8 | uint16(b[off+1])
	return sh, nil
}

// AppendServerHello 拼一个 ServerHello。
func AppendServerHello(dst []byte, sh *ServerHello, keyShare []byte) []byte {
	body := make([]byte, 0, 128)
	body = append(body, byte(sh.LegacyVersion>>8), byte(sh.LegacyVersion))
	body = append(body, sh.Random[:]...)

	body = append(body, byte(len(sh.SessionID)))
	body = append(body, sh.SessionID...)

	body = append(body, byte(sh.CipherSuite>>8), byte(sh.CipherSuite))
	body = append(body, 0) // compression_methods: 空

	var exts []byte
	// supported_versions：选 1.3
	exts = appendExtension(exts, extSupportedVersions, []byte{0x03, 0x04})
	// key_share：服务端的公钥
	exts = appendExtension(exts, extKeyShare, appendKeyShare(keyShare))

	body = append(body, byte(len(exts)>>8), byte(len(exts)))
	body = append(body, exts...)

	return AppendHandshake(dst, hsServerHello, body)
}

// ---------------------------------------------------------------------------
// 扩展

const (
	extServerName          uint16 = 0
	extSupportedGroups     uint16 = 10
	extSignatureAlgorithms uint16 = 13
	extSupportedVersions   uint16 = 43
	extKeyShare            uint16 = 51
)

// parseExtensions 遍历扩展列表（每项是 2 字节类型 + 2 字节长度 + 数据）。
func parseExtensions(b []byte, fn func(typ uint16, data []byte)) {
	for len(b) >= 4 {
		typ := uint16(b[0])<<8 | uint16(b[1])
		length := int(b[2])<<8 | int(b[3])
		if 4+length > len(b) {
			return
		}
		fn(typ, b[4:4+length])
		b = b[4+length:]
	}
}

// appendExtension 追加一个扩展。
func appendExtension(dst []byte, typ uint16, data []byte) []byte {
	dst = append(dst, byte(typ>>8), byte(typ), byte(len(data)>>8), byte(len(data)))
	return append(dst, data...)
}

// appendKeyShare 拼 key_share 扩展的内容（一个 x25519 的公钥）。
func appendKeyShare(pub []byte) []byte {
	// client_shares: 2 字节列表长度 + (2 字节组 + 2 字节长度 + 公钥)
	entry := make([]byte, 0, 4+len(pub))
	entry = append(entry, 0, byte(x25519Group), byte(len(pub)>>8), byte(len(pub)))
	entry = append(entry, pub...)

	out := make([]byte, 0, 2+len(entry))
	out = append(out, byte(len(entry)>>8), byte(len(entry)))
	return append(out, entry...)
}

// x25519Group 是 x25519 的命名组编号。
const x25519Group = 0x001d

// appendSupportedGroups 拼 supported_groups 扩展（RFC 8446 4.2.7）。
//
// 结构：2 字节列表长度 + 每项 2 字节的组号。
//
// 我们只做 x25519，就只声明它。
func appendSupportedGroups() []byte {
	groups := []uint16{x25519Group}
	out := make([]byte, 0, 2+len(groups)*2)
	out = append(out, byte(len(groups)*2>>8), byte(len(groups)*2))
	for _, g := range groups {
		out = append(out, byte(g>>8), byte(g))
	}
	return out
}

// appendSignatureAlgorithms 拼 signature_algorithms 扩展。
//
// 只需要能被服务端接受——我们校验证书时用的是证书自带的算法，不靠这个
// 列表选。
func appendSignatureAlgorithms() []byte {
	// 列表长度 + (rsa_pss_rsae_sha256, ecdsa_secp256r1_sha256, ed25519)
	algs := []uint16{0x0804, 0x0403, 0x0807}
	out := make([]byte, 0, 2+len(algs)*2)
	out = append(out, byte(len(algs)*2>>8), byte(len(algs)*2))
	for _, a := range algs {
		out = append(out, byte(a>>8), byte(a))
	}
	return out
}

// appendServerName 拼 server_name 扩展。
func appendServerName(name string) []byte {
	out := make([]byte, 0, 5+len(name))
	inner := make([]byte, 0, 3+len(name))
	inner = append(inner, 0) // 类型 0 = host_name
	inner = append(inner, byte(len(name)>>8), byte(len(name)))
	inner = append(inner, name...)
	out = append(out, byte(len(inner)>>8), byte(len(inner)))
	return append(out, inner...)
}

// ---------------------------------------------------------------------------
// ECDHE 密钥交换

// keyExchange 是一次 ECDHE 交换的状态。
type keyExchange struct {
	priv *ecdh.PrivateKey
	pub  []byte
}

// newKeyExchange 生成一对 x25519 密钥。
func newKeyExchange() (*keyExchange, error) {
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &keyExchange{priv: priv, pub: priv.PublicKey().Bytes()}, nil
}

// shared 用对端的公钥算出共享密钥。
func (k *keyExchange) shared(peerPub []byte) ([]byte, error) {
	pub, err := ecdh.X25519().NewPublicKey(peerPub)
	if err != nil {
		return nil, fmt.Errorf("tls: bad peer public key: %w", err)
	}
	return k.priv.ECDH(pub)
}

// parseKeyShare 从 key_share 扩展里取出对端的公钥。
func parseKeyShare(data []byte) ([]byte, error) {
	// 如果是 ServerHello 的（单个 entry）：2 字节组 + 2 字节长度 + 公钥
	if len(data) < 4 {
		return nil, errors.New("tls: key_share too short")
	}
	group := uint16(data[0])<<8 | uint16(data[1])
	if group != x25519Group {
		return nil, fmt.Errorf("tls: unsupported group %#x", group)
	}
	length := int(data[2])<<8 | int(data[3])
	if 4+length > len(data) {
		return nil, errors.New("tls: key_share truncated")
	}
	return data[4 : 4+length], nil
}

// clientKeyShare 从 ClientHello 的原始字节里扫出 key_share 扩展的公钥。
//
// 为什么要重新扫一遍：ParseClientHello 只解析了它关心的字段（版本、
// 随机数、套件、SNI），key_share 的内容是二进制结构，放在这里单独处理
// 更清楚。
//
// ClientHello 的结构（RFC 8446 4.1.2）：
//
//	legacy_version(2) random(32) session_id(1+n) cipher_suites(2+n)
//	compression_methods(1+n) extensions(2+n)
func clientKeyShare(ch []byte) ([]byte, error) {
	off := 34 // version + random

	if off >= len(ch) {
		return nil, ErrBadClientHello
	}
	sidLen := int(ch[off])
	off += 1 + sidLen

	if off+2 > len(ch) {
		return nil, ErrBadClientHello
	}
	csLen := int(ch[off])<<8 | int(ch[off+1])
	off += 2 + csLen

	if off >= len(ch) {
		return nil, ErrBadClientHello
	}
	cmLen := int(ch[off])
	off += 1 + cmLen

	return findKeyShare(ch, off)
}

// serverKeyShare 从 ServerHello 的原始字节里扫出 key_share 扩展。
func serverKeyShare(sh []byte) ([]byte, error) {
	off := 34

	if off >= len(sh) {
		return nil, ErrBadServerHello
	}
	sidLen := int(sh[off])
	off += 1 + sidLen

	// cipher_suite(2) + compression_method(1)
	off += 3

	return findKeyShare(sh, off)
}

// findKeyShare 从 off 开始（extensions 的 2 字节长度字段）找 key_share，
// 返回 x25519 的公钥。
//
// **客户端和服务端的 key_share 结构不一样**（RFC 8446 4.2.8）：
//
//	ClientHello（列表）：
//	  client_shares<0..2^16-1>
//	    若干 entry: group(2) + key_exchange<1..2^16-1>
//
//	ServerHello（单个）：
//	  group(2) + key_exchange<1..2^16-1>
//
// 服务端那个**没有外面的列表长度**。早先统一按"列表"解，解析标准库的
// ServerHello 时报 "client key_share entry truncated"——它把单个 entry
// 的前两个字节（组号 00 1d）当成了"列表长度 29"，然后按这个去找 entry，
// 自然对不上。
func findKeyShare(b []byte, off int) ([]byte, error) {
	if off+2 > len(b) {
		return nil, errors.New("tls: no extensions")
	}
	extLen := int(b[off])<<8 | int(b[off+1])
	off += 2
	if off+extLen > len(b) {
		return nil, errors.New("tls: extensions truncated")
	}

	var found []byte
	var findErr error
	parseExtensions(b[off:off+extLen], func(typ uint16, data []byte) {
		if typ != extKeyShare || found != nil || findErr != nil {
			return
		}
		// 先按 ServerHello 的格式试（单个 entry）：组号 + 长度 + 公钥。
		// 对不上的话再按 ClientHello 的列表格式找。
		found, findErr = parseKeyShareEntry(data)
		if findErr != nil {
			found, findErr = parseClientKeyShare(data)
		}
	})
	if findErr != nil {
		return nil, findErr
	}
	if found == nil {
		return nil, errors.New("tls: no key_share extension")
	}
	return found, nil
}

// parseKeyShareEntry 按"单个 entry"解 key_share（ServerHello 的格式）。
func parseKeyShareEntry(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, errors.New("tls: key_share entry too short")
	}
	group := uint16(data[0])<<8 | uint16(data[1])
	if group != x25519Group {
		return nil, fmt.Errorf("tls: unsupported group %#x", group)
	}
	length := int(data[2])<<8 | int(data[3])
	if 4+length > len(data) {
		return nil, errors.New("tls: key_share entry truncated")
	}
	return data[4 : 4+length], nil
}

// parseClientKeyShare 从 ClientHello 的 key_share 里取第一个 x25519 公钥。
func parseClientKeyShare(data []byte) ([]byte, error) {
	if len(data) < 2 {
		return nil, errors.New("tls: client key_share too short")
	}
	// client_shares: 2 字节总长度 + 若干 entry
	total := int(data[0])<<8 | int(data[1])
	if 2+total > len(data) {
		return nil, errors.New("tls: client key_share truncated")
	}
	b := data[2 : 2+total]
	for len(b) >= 4 {
		group := uint16(b[0])<<8 | uint16(b[1])
		length := int(b[2])<<8 | int(b[3])
		if 4+length > len(b) {
			return nil, errors.New("tls: client key_share entry truncated")
		}
		if group == x25519Group {
			return b[4 : 4+length], nil
		}
		b = b[4+length:]
	}
	return nil, errors.New("tls: no x25519 key share")
}
