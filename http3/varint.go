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

// Package http3 是 fio 上的 HTTP/3。
//
// HTTP/3 跑在 QUIC 上（RFC 9000），而 QUIC 跑在 UDP 上。**它和其他协议
// 最根本的不同是：没有内核帮忙管连接**。TCP 里连接状态、重传、拥塞控制、
// 流的顺序都是内核做的；QUIC 把这些全搬到用户态——因为 UDP 什么都不管，
// 丢包、乱序、重复都归应用处理。
//
// 所以 HTTP/3 是这套东西里唯一**不走 epoll 读事件**的协议：
//
//	TCP 协议                    HTTP/3
//	epoll 说可读              UDP socket 收包（也是 epoll，但收到的是一个
//	  -> read 一次拿到一串       一个数据报，不是字节流）
//	  -> 按字节流切帧          -> 每个包自己带连接 ID 和包号
//	                          -> 自己按包号排序、自己发 ACK、自己重传
//
// 它仍然用 engine 的缓冲区和任务池，但事件源是自己的 UDP 收包循环。
//
// 这个包先做**能验证的部分**：QUIC 的变长整数、包头的解析和生成、HTTP/3
// 的帧层。完整的 QUIC（握手、丢包恢复、拥塞控制、流控）是一个量级很大
// 的东西，不是几个文件能盖住的——那部分要么自己写一个完整的 QUIC 栈，
// 要么用现成的（quic-go）。这里把协议的数据结构做出来，让"HTTP/3 的帧长
// 什么样"这件事有代码可依。
package http3

import "errors"

// QUIC 的变长整数（RFC 9000 section 16）。
//
// 前两个位表示后面还有几个字节：
//
//	0b00  1 字节，值占 6 位   （0..63）
//	0b01  2 字节，值占 14 位  （0..16383）
//	0b10  4 字节，值占 30 位  （0..1073741823）
//	0b11  8 字节，值占 62 位  （0..4611686018427387903）
//
// 为什么不用定长：包号、流 ID、长度这些值大小差别很大（流 ID 可能是个位
// 数，长度可能是 MB），定长要么浪费字节要么不够用。变长让小的值只占
// 一个字节。
var (
	ErrVarintTooShort = errors.New("http3: varint truncated")
	ErrVarintTooLong  = errors.New("http3: varint value too large")
)

// VarintLen 返回编码 v 需要几个字节。
func VarintLen(v uint64) int {
	switch {
	case v < 1<<6:
		return 1
	case v < 1<<14:
		return 2
	case v < 1<<30:
		return 4
	case v < 1<<62:
		return 8
	}
	return 0 // v 太大，放不下
}

// AppendVarint 把 v 按 QUIC 的变长整数编码追加到 dst 上。
//
// v 超过 2^62-1 时原样返回 dst（调用方该自己保证不越界；QUIC 里没有
// 这么大的字段）。
func AppendVarint(dst []byte, v uint64) []byte {
	switch {
	case v < 1<<6:
		return append(dst, byte(v))
	case v < 1<<14:
		return append(dst, byte(v>>8)|0x40, byte(v))
	case v < 1<<30:
		return append(dst, byte(v>>24)|0x80, byte(v>>16), byte(v>>8), byte(v))
	case v < 1<<62:
		return append(dst,
			byte(v>>56)|0xc0, byte(v>>48), byte(v>>40), byte(v>>32),
			byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
	}
	return dst
}

// ReadVarint 从 b 里读一个变长整数，返回值和读了多少字节。
//
// 不够读就返回 ErrVarintTooShort——这是非阻塞 io 的常态（UDP 包虽然是
// 整包到的，但同一个包里的字段仍然可能被上层切着解读）。
func ReadVarint(b []byte) (v uint64, n int, err error) {
	if len(b) == 0 {
		return 0, 0, ErrVarintTooShort
	}
	prefix := b[0] >> 6
	size := 1 << prefix // 1, 2, 4, 8
	if len(b) < size {
		return 0, 0, ErrVarintTooShort
	}
	// 清掉前缀位，剩下的按大端拼
	v = uint64(b[0] & 0x3f)
	for i := 1; i < size; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, size, nil
}

// AppendVarintLen 写"长度 + 内容"：先写长度，再写内容。
//
// QUIC 里到处是这个模式（帧里的字段、HTTP/3 的头块）。
func AppendVarintLen(dst []byte, content []byte) []byte {
	dst = AppendVarint(dst, uint64(len(content)))
	return append(dst, content...)
}

// ReadVarintLen 读"长度 + 内容"，返回内容和读了多少字节。
func ReadVarintLen(b []byte) (content []byte, n int, err error) {
	l, m, err := ReadVarint(b)
	if err != nil {
		return nil, 0, err
	}
	if uint64(len(b)-m) < l {
		return nil, 0, ErrVarintTooShort
	}
	return b[m : m+int(l)], m + int(l), nil
}
