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
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// 服务端和客户端。**QUIC 那层用 quic-go**，不自己写。
//
// 为什么：完整的 QUIC 是一个协议栈的量级——TLS 1.3 握手集成（QUIC 的
// TLS 不走 TLS 记录层，密钥直接从握手导出）、丢包检测和重传、RTT 估计、
// 拥塞控制（NewReno/CUBIC）、流控（连接级和流级两套窗口）、连接迁移、
// 0-RTT。这些每一样都够写很久，而且写错了症状是"某些网络环境下偶发卡顿"，
// 极难查。
//
// 这个包的 **varint/packet/frame** 那部分是自己实现的（协议的数据结构，
// 看得见摸得着，也测过），quic-go 负责把包可靠地在 UDP 上搬来搬去。
// 分工是：我们懂 HTTP/3 的帧长什么样，quic-go 懂怎么把字节送到对面。
//
// 这也是"能基于 epoll 就基于 epoll"的例外：HTTP/3 跑在 UDP 上，没有
// TCP 连接给内核管，epoll 在这里帮不上忙（它只能告诉你 UDP 套接字可读，
// 剩下的事——排序、重传、拥塞——都得自己做）。

// Server 是一个 HTTP/3 服务端。
type Server struct {
	// Addr 是监听的 UDP 地址，形如 ":443"。空的话用 ":443"。
	Addr string

	// Handler 和 net/http 的 Handler 一样，处理请求。
	//
	// **用 net/http 的 Handler 而不是自己定义**：HTTP/3 在语义上和
	// HTTP/1.1、HTTP/2 是一回事（都有 Request、ResponseWriter），
	// 借用这个接口意味着已有的 handler 直接能用。
	Handler http.Handler

	// TLSConfig 至少要有证书。QUIC 强制 TLS 1.3。
	TLSConfig *tls.Config

	// QUICConfig 是 QUIC 那层的参数（连接数上限、空闲超时这些）。
	QUICConfig *quic.Config

	// server 是底层的 quic-go 服务端
	server *http3.Server
	mu     sync.Mutex
	// conn 是监听用的 UDP 套接字（自己做的话要拿它收包）
	conn net.PacketConn
}

// ListenAndServe 起服务，阻塞到出错或者被关。
func (s *Server) ListenAndServe() error {
	s.mu.Lock()
	s.server = &http3.Server{
		Addr:       s.addr(),
		Handler:    s.Handler,
		TLSConfig:  s.TLSConfig,
		QUICConfig: s.QUICConfig,
	}
	srv := s.server
	s.mu.Unlock()
	return srv.ListenAndServe()
}

// ListenAndServePacketConn 用一个已经建好的 UDP 套接字起服务。
//
// 这个更贴近"自己控制收包"的用法：套接字可以设成非阻塞、可以自己
// 挂 epoll 拿"有包到了"的通知（虽然 QUIC 的重传排序还是 quic-go 做）。
func (s *Server) ListenAndServePacketConn(pc net.PacketConn) error {
	s.mu.Lock()
	s.conn = pc
	s.server = &http3.Server{
		Handler:    s.Handler,
		TLSConfig:  s.TLSConfig,
		QUICConfig: s.QUICConfig,
	}
	srv := s.server
	s.mu.Unlock()
	return srv.Serve(pc)
}

// Close 关服务。
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.server
	s.mu.Unlock()
	if srv != nil {
		return srv.Close()
	}
	return nil
}

func (s *Server) addr() string {
	if s.Addr == "" {
		return ":443"
	}
	return s.Addr
}

// ---------------------------------------------------------------------------
// 客户端

// Transport 是一个 HTTP/3 的 http.RoundTripper。
//
// 有了它，标准的 http.Client 就能走 HTTP/3：
//
//	client := &http.Client{Transport: &http3.Transport{}}
//	resp, err := client.Get("https://example.com/")
type Transport struct {
	// TLSConfig 至少要能验证对端证书
	TLSConfig *tls.Config
	// QUICConfig 是 QUIC 那层的参数
	QUICConfig *quic.Config

	transport *http3.Transport
	once      sync.Once
}

func (t *Transport) init() {
	t.transport = &http3.Transport{
		TLSClientConfig: t.TLSConfig,
		QUICConfig:      t.QUICConfig,
	}
}

// RoundTrip 实现 http.RoundTripper。
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.once.Do(t.init)
	return t.transport.RoundTrip(req)
}

// Close 关掉所有连接。
func (t *Transport) Close() error {
	t.once.Do(t.init)
	if t.transport != nil {
		return t.transport.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// 用我们自己的帧层解析 QUIC 流

// StreamReader 把一条 QUIC 流里的 HTTP/3 帧解出来。
//
// 这一层是**我们自己实现的**（见 frame.go）：quic-go 给的是"一条可靠的
// 字节流"，帧的边界要自己找。用法：
//
//	sr := NewStreamReader(stream)
//	for {
//	    f, err := sr.Next()
//	    if err != nil { break }   // io.EOF 表示流结束了
//	    // f.Type / f.Payload
//	}
type StreamReader struct {
	r     io.Reader
	parse *FrameParser
	// buf 是给底层读用的
	buf []byte
	// frames 是这次读解出来的帧
	frames []Frame
	// idx 是下次返回第几个
	idx int
	// err 是记住的错误
	err error
}

// NewStreamReader 包一条 QUIC 流。
func NewStreamReader(r io.Reader) *StreamReader {
	return &StreamReader{
		r:     r,
		parse: NewFrameParser(),
		buf:   make([]byte, 16*1024),
	}
}

// Next 返回下一个帧。流结束时返回 io.EOF。
func (sr *StreamReader) Next() (*Frame, error) {
	for {
		if sr.idx < len(sr.frames) {
			f := &sr.frames[sr.idx]
			sr.idx++
			return f, nil
		}
		if sr.err != nil {
			return nil, sr.err
		}

		// 读一块，解帧
		n, err := sr.r.Read(sr.buf)
		if n > 0 {
			sr.frames = sr.frames[:0]
			sr.idx = 0
			// 解析器自己会攒，所以这里每次喂新读到的
			if _, perr := sr.parse.Parse(sr.buf[:n], func(f *Frame) error {
				sr.frames = append(sr.frames, Frame{
					Type:    f.Type,
					Payload: append([]byte(nil), f.Payload...),
				})
				return nil
			}); perr != nil {
				sr.err = perr
				return nil, perr
			}
			if len(sr.frames) > 0 {
				continue
			}
		}
		if err != nil {
			sr.err = err
			return nil, err
		}
	}
}

// ---------------------------------------------------------------------------
// 便捷：用标准库的 http.Client 走 HTTP/3

// NewClient 建一个走 HTTP/3 的 http.Client。
func NewClient(tlsConfig *tls.Config) *http.Client {
	tr := &Transport{TLSConfig: tlsConfig}
	return &http.Client{Transport: tr}
}

// ListenUDP 建一个 UDP 套接字（可以自己设非阻塞、自己挂 epoll）。
func ListenUDP(addr string) (net.PacketConn, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("http3: resolve %s: %w", addr, err)
	}
	return net.ListenUDP("udp", ua)
}
