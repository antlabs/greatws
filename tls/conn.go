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
	"crypto/tls"
	"errors"
	"sync"
)

// ErrHandshakeIncomplete 握手还没完成就调了 Write。
var ErrHandshakeIncomplete = errors.New("tls: handshake is not complete")

// Conn 是一条 TLS 连接。
//
// 握手阶段：TLS 跑在一个专门的 goroutine 里。**为什么不是状态机**——
// crypto/tls 的握手不支持分步：它内部的 handshakeErr 一旦置上，后面每次
// 调用都直接返回那个错误，不会再尝试读（见 GOROOT 的 crypto/tls/conn.go：
// "if err := c.handshakeErr; err != nil { return err }"）。所以"喂一段
// 数据、推进一点"这种做法在它上面行不通——第一次返回 need more data
// 这条连接就废了。实测过，纯 crypto/tls 上就能复现。
//
// 做法是把阻塞挪到别处：握手放独立 goroutine，memConn.Read 在那个
// goroutine 里等 channel。事件循环那边只管 Feed 和 Take，**从不阻塞**。
// 握手完成之后 goroutine 退出，加解密直接在调用方 goroutine 上跑。
//
// 用法（事件循环的 OnData 里）：
//
//	tc.Feed(ciphertext)   // 喂进去，不阻塞
//	if out := tc.Take(); len(out) > 0 {
//	    conn.Write(out)
//	}
//	if tc.HandshakeDone() {
//	    plain := tc.Read()
//	    // 交给上层协议（http、websocket...）
//	}
type Conn struct {
	mem *memConn
	tc  *tls.Conn

	mu sync.Mutex

	// handshakeDone 握手跑完了（成功或失败）
	handshakeDone bool
	// handshakeErr 握手失败的原因
	handshakeErr error

	// plain 是解出来的明文，攒着等 Read 取
	plain []byte
	// readBuf 是给 tls.Conn.Read 用的中转
	readBuf []byte

	// hsDone 是握手 goroutine 退出时关的
	hsDone chan struct{}
}

// Config 是 TLS 的配置，直接用 crypto/tls 的。
type Config = tls.Config

// Server 建一条服务端 TLS 连接。config 至少要有 Certificates。
func Server(config *Config) *Conn {
	m := newMemConn()
	return newConn(tls.Server(m, config), m)
}

// Client 建一条客户端 TLS 连接。
func Client(config *Config) *Conn {
	m := newMemConn()
	return newConn(tls.Client(m, config), m)
}

func newConn(tc *tls.Conn, m *memConn) *Conn {
	c := &Conn{
		tc:     tc,
		mem:    m,
		hsDone: make(chan struct{}),
	}
	go c.handshake()
	return c
}

// handshake 在独立 goroutine 里跑完整握手。
//
// 它里面的 memConn.Read 会阻塞等数据，但阻塞的是这个 goroutine，不是
// 事件循环。握手完成（或失败）之后退出。
func (c *Conn) handshake() {
	defer close(c.hsDone)

	err := c.tc.Handshake()

	c.mu.Lock()
	c.handshakeDone = true
	c.handshakeErr = err
	c.mu.Unlock()
}

// Feed 把从 fd 上读到的密文喂进来。不阻塞。
func (c *Conn) Feed(ciphertext []byte) { c.mem.Feed(ciphertext) }

// Take 取走要发出去的密文。不阻塞。
func (c *Conn) Take() []byte { return c.mem.Take() }

// Pending 还有多少密文等着被 TLS 吃。
func (c *Conn) Pending() int { return c.mem.Pending() }

// HandshakeDone 握手跑完了没有（成功或失败）。
func (c *Conn) HandshakeDone() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handshakeDone
}

// HandshakeError 握手失败的原因，没失败返回 nil。
func (c *Conn) HandshakeError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.handshakeErr
}

// WaitHandshake 等握手结束。**会阻塞**——只有调用方自己愿意等（测试、
// 或者启动阶段的同步握手）才调。事件循环里不要调。
func (c *Conn) WaitHandshake() error {
	<-c.hsDone
	return c.HandshakeError()
}

// Read 取已经解出来的明文。不阻塞：有多少给多少。
//
// 返回的切片所有权归调用方。每次 Feed 之后调它，把这次解出来的明文
// 拿走。
//
// **只在有密文待解的时候才动 tls.Conn.Read**：那个调用在内部缓冲读空
// 之后会去 memConn 要更多数据，而 memConn.Read 是会阻塞的（见
// memconn.go）——在事件循环线程上阻塞就把整个循环卡住了。所以先看
// Pending()，没密文就直接返回。
func (c *Conn) Read() []byte {
	if err := c.HandshakeError(); err != nil {
		return nil
	}
	if !c.HandshakeDone() {
		return nil
	}
	if c.mem.Pending() == 0 {
		return nil
	}

	if c.readBuf == nil {
		c.readBuf = make([]byte, 16*1024)
	}
	// 读到"取不出更多明文"为止。
	//
	// 两个来源都要读空：
	//   1. 密文还没解完（mem.Pending() > 0）——一个 64KB 的消息跨好几个
	//      TLS 记录，得读很多次
	//   2. TLS 内部缓冲里还有解好的明文——一次 tc.Read 只给一块
	//      （readBuf 那么大），密文吃完了明文可能还没取完
	//
	// 只判第一条的话，大消息只拿到第一块（实测 64KB 只能拿到 65230
	// 字节，恰好是"密文吃完了、明文还剩 306 字节在 TLS 内部"）。
	//
	// **tc.Read 会阻塞**：内部缓冲空了它就去 memConn 要更多。所以不能
	// 无脑循环 —— 每一轮都先确认"还有东西可解"，没有就停。
	for c.mem.Pending() > 0 {
		n, err := c.tc.Read(c.readBuf)
		if n > 0 {
			c.plain = append(c.plain, c.readBuf[:n]...)
		}
		if err != nil {
			break
		}
		if n == 0 {
			break // 没有进展，防死循环
		}
	}
	// 密文吃完了，TLS 内部可能还剩解好的明文（一次 tc.Read 只给一块，
	// readBuf 那么大）。要用**非阻塞**的方式把它取出来——pending 为 0
	// 的时候 memConn.Read 会等 channel，直接调 tc.Read 就把调用方卡住了
	// （实测：事件循环线程上卡死，测试 90 秒超时）。
	//
	// 所以先把 memConn 标记成"到此为止"（塞一个空标记），让它别阻塞；
	// 内部有明文的话 tc.Read 会先给明文，没有才去要密文，那时拿到的是
	// 空 → 返回 io.EOF，循环结束。
	c.drainPlain()

	if len(c.plain) == 0 {
		return nil
	}
	out := c.plain
	c.plain = nil
	return out
}

// drainPlain 把 TLS 内部缓冲里还剩的明文取出来，不阻塞。
//
// TLS 每收一条记录就解一次，但 tc.Read 一次只返回一块（调用方给的
// 缓冲区那么大），所以一条 64KB 的消息可能要读好几次才吐完。密文都吃
// 完之后，内部还留着最后那一块。
//
// 让它非阻塞的办法：memConn 上有 eof 标志，置上之后 Read 立刻返回
// io.EOF 而不是等 channel。tc.Read 拿到 io.EOF 之前会把现成的明文给
// 出来，所以循环会自然地读完再停。
func (c *Conn) drainPlain() {
	c.mem.setDraining(true)
	defer c.mem.setDraining(false)

	for {
		n, err := c.tc.Read(c.readBuf)
		if n > 0 {
			c.plain = append(c.plain, c.readBuf[:n]...)
		}
		if err != nil {
			break
		}
		if n == 0 {
			break
		}
	}
}

// Write 把明文加密，返回要发出去的密文。
//
// 握手没完成时返回 ErrHandshakeIncomplete——正常流程是先握手再传数据。
func (c *Conn) Write(plain []byte) ([]byte, error) {
	if err := c.HandshakeError(); err != nil {
		return nil, err
	}
	if !c.HandshakeDone() {
		return nil, ErrHandshakeIncomplete
	}
	if _, err := c.tc.Write(plain); err != nil {
		return c.mem.Take(), err
	}
	return c.mem.Take(), nil
}

// ConnectionState 暴露底层 TLS 的状态（协商的版本、密码套件、对端证书）。
func (c *Conn) ConnectionState() tls.ConnectionState {
	return c.tc.ConnectionState()
}

// Close 关连接：让等着的握手 goroutine 退出，发 close_notify。
func (c *Conn) Close() ([]byte, error) {
	c.mem.Close()
	if err := c.tc.Close(); err != nil {
		return c.mem.Take(), err
	}
	return c.mem.Take(), nil
}
