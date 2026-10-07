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

// Package tls 是 fio 上的 TLS，用状态机实现，不阻塞事件循环。
//
// 为什么不直接用 crypto/tls.Conn：那个 Conn 是**阻塞语义**的——它的
// Read 要么给你够要求的字节、要么一直等到够（或者出错）。套在非阻塞 fd
// 上，一次 Read 就会把整个事件循环卡住，一个连接握手到一半能把所有连接
// 都拖死。
//
// 做法：给 crypto/tls 一个**内存连接**（memConn），它的 Read 从我们喂
// 进去的密文里取、Write 把要发的密文吐出来，两端都是内存。真正的 fd
// 读写在事件循环那边——密文到了喂进 memConn，memConn 吐出来的密文交给
// 引擎写出去。
//
// 这样 crypto/tls 那套经过审计的握手、记录层、密钥交换全都能用，而
// "什么时候读、什么时候写"还是我们的 epoll 说了算。
//
// 用法（服务端）：
//
//	// 收到密文就喂进去
//	tc.Feed(ciphertext)
//	// 让它跑一步握手/解密，返回值是要发出去的密文
//	out, err := tc.Handshake()
//	conn.Write(out)
package tls

import (
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ErrWantMore 表示"数据还不够，再给我一点"。
//
// 这是这套东西的核心：非阻塞 io 上没有"等一会儿"这个操作，只能返回
// 一个"现在不行"的信号，让调用方下次再来。
var ErrWantMore = errors.New("tls: need more data")

// ErrWantWrite 表示"我要写，但写不出去"。
var ErrWantWrite = errors.New("tls: want write")

// memConn 是 crypto/tls 和事件循环之间的那一层：TLS 从它读密文、往它
// 写密文，两边都是内存。
//
// **为什么 Read 会阻塞（用 channel 等）**：crypto/tls 的握手不是分步的
// ——它的 handshakeErr 一旦置上，后面每次调用都直接返回那个错误，不会再
// 尝试读（见 GOROOT/src/crypto/tls/conn.go 的 "if err := c.handshakeErr;
// err != nil { return err }"）。所以"喂一段数据、推进一点"这种非阻塞
// 握手在 crypto/tls 上做不到：第一次返回 need more data 它就废了。
//
// 做法是把阻塞挪到别的地方：握手跑在一个专门的 goroutine 里，memConn.Read
// 在那个 goroutine 里等 channel，等到 Feed 送数据进来才返回。事件循环
// 那边完全不受影响——它只管 Feed 和 Take，从不阻塞。
//
//	事件循环                         握手 goroutine
//	OnData: mem.Feed(密文)  ------>  Read 返回，TLS 继续跑
//	         mem.Take()    <------  Write 攒下要发的密文
//
// 一个连接要么在握手 goroutine 里，要么已经握手完、直接在事件循环里
// 加解密，不会同时。
type memConn struct {
	// mu 保护 out。握手的 goroutine 往 out 里写，事件循环那边 Take 走，
	// 两边是并发的。
	mu sync.Mutex
	// in 是喂进来但还没被 TLS 吃掉的密文
	in []byte
	// out 是 TLS 要发出去的密文
	out []byte

	// inCh 让握手的 goroutine 等数据。Feed 往里塞，Read 从里取。
	inCh chan []byte
	// draining 置上之后 Read 不再等 channel，直接返回 io.EOF。
	//
	// 用在"密文都吃完了，把 TLS 内部还剩的明文取出来"这个阶段：那时候
	// 确实不该再等了，等就是卡住调用方。
	draining atomic.Bool

	// queued 是 channel 里还没被取出来的字节数（原子读写）。
	//
	// 为什么要单独记一个数：Pending() 要能回答"还有多少密文没解"，而
	// 光看 m.in 是不知道 channel 里排了多少的。去 channel 里"取一个看
	// 一眼再放回去"不行——那样会打乱顺序（Feed 进来的记录必须按序交给
	// TLS，记录层是有序的）。
	queued int64
	// closed 关掉之后 Read 返回 io.EOF，让握手的 goroutine 退出。
	closed chan struct{}
}

func newMemConn() *memConn {
	return &memConn{
		inCh:   make(chan []byte, 16),
		closed: make(chan struct{}),
	}
}

// Feed 把从 fd 读到的密文喂进来。
//
// 不阻塞：塞进 channel 就走。channel 满了说明对端发得比 TLS 吃得快，
// 那是流量控制的事（缓冲区本来就是用来吸收这个的）。
func (m *memConn) Feed(b []byte) {
	if len(b) == 0 {
		return
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case m.inCh <- cp:
		atomic.AddInt64(&m.queued, int64(len(cp)))
	case <-m.closed:
	}
}

// Take 取走 TLS 要发出去的密文。不阻塞。
func (m *memConn) Take() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.out) == 0 {
		return nil
	}
	out := m.out
	m.out = nil
	return out
}

// Pending 还有多少密文等着被 TLS 吃。
//
// 两部分：已经取出来还没吃完的（m.in），加上还在 channel 里排队的
// （queued）。
func (m *memConn) Pending() int {
	return len(m.in) + int(atomic.LoadInt64(&m.queued))
}

// setDraining 开关"不等数据"模式，见 draining。
func (m *memConn) setDraining(v bool) { m.draining.Store(v) }

// Close 让等着的 Read 醒过来（返回 io.EOF）。
func (m *memConn) Close() error {
	select {
	case <-m.closed:
	default:
		close(m.closed)
	}
	return nil
}

// Read 让 TLS 从这里拿密文。
//
// **会阻塞**，但只在握手的 goroutine 里——见 memConn 的说明。有数据已经
// 攒着的时候不阻塞，直接给。
func (m *memConn) Read(p []byte) (int, error) {
	if len(m.in) == 0 {
		if m.draining.Load() {
			return 0, io.EOF
		}
		select {
		case b := <-m.inCh:
			atomic.AddInt64(&m.queued, -int64(len(b)))
			m.in = b
		case <-m.closed:
			return 0, io.EOF
		}
	}
	n := copy(p, m.in)
	m.in = m.in[n:]
	if len(m.in) == 0 {
		m.in = nil
	}
	return n, nil
}

// Write 收下 TLS 要发的密文。
func (m *memConn) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.out = append(m.out, p...)
	return len(p), nil
}

func (m *memConn) LocalAddr() net.Addr              { return memAddr{} }
func (m *memConn) RemoteAddr() net.Addr             { return memAddr{} }
func (m *memConn) SetDeadline(time.Time) error      { return nil }
func (m *memConn) SetReadDeadline(time.Time) error  { return nil }
func (m *memConn) SetWriteDeadline(time.Time) error { return nil }

type memAddr struct{}

func (memAddr) Network() string { return "mem" }
func (memAddr) String() string  { return "mem" }

// 让 io 被引用（错误值和它有关）
var _ = io.EOF
