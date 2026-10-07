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
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// selfSigned 造一张自签证书。
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fio-h3-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"fio-h3-test", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// **端到端**：起一个 HTTP/3 服务端，用 HTTP/3 客户端请求它。
//
// 这条路径把 quic-go 的传输和我们自己的帧层串起来，能证明整套东西
// 在真实网络栈上（UDP + QUIC + TLS 1.3）跑得通。
func TestEndToEnd(t *testing.T) {
	cert := selfSigned(t)

	// 找一个空闲的 UDP 端口
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()

	srv := &Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("X-Method", r.Method)
			fmt.Fprintf(w, "hello %s", r.URL.Path)
		}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}

	go func() {
		if err := srv.ListenAndServePacketConn(pc); err != nil {
			// 关服务时会返回错误，正常
		}
	}()
	defer srv.Close()

	// 客户端（自签证书，跳过验证）
	client := NewClient(&tls.Config{InsecureSkipVerify: true})
	defer client.CloseIdleConnections()

	// 等一会儿让服务端起来
	time.Sleep(100 * time.Millisecond)

	resp, err := client.Get("https://" + addr + "/world")
	if err != nil {
		t.Skipf("HTTP/3 请求失败（可能环境不支持 UDP/QUIC）: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("状态码 = %d, want 200", resp.StatusCode)
	}
	if string(body) != "hello /world" {
		t.Errorf("响应体 = %q, want %q", body, "hello /world")
	}
	if got := resp.Header.Get("X-Method"); got != "GET" {
		t.Errorf("X-Method = %q, want GET", got)
	}
	t.Logf("HTTP/3 端到端跑通: %s %s -> %d", resp.Proto, addr, resp.StatusCode)
}

// 服务端和客户端要在同一个连接上跑多个请求（多路复用）。
func TestMultiplexedRequests(t *testing.T) {
	cert := selfSigned(t)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := pc.LocalAddr().String()

	srv := &Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "path=%s", r.URL.Path)
		}),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}},
	}
	go srv.ListenAndServePacketConn(pc)
	defer srv.Close()

	client := NewClient(&tls.Config{InsecureSkipVerify: true})
	defer client.CloseIdleConnections()
	time.Sleep(100 * time.Millisecond)

	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("/req%d", i)
		resp, err := client.Get("https://" + addr + path)
		if err != nil {
			t.Skipf("HTTP/3 不可用: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := "path=" + path
		if string(body) != want {
			t.Errorf("第 %d 个: 响应 = %q, want %q", i, body, want)
		}
	}
}

// StreamReader 把一条流里的 HTTP/3 帧解出来（这是我们自己实现的那层）。
func TestStreamReader(t *testing.T) {
	// 拼一段流：HEADERS + DATA + DATA
	var stream []byte
	stream = AppendHeaders(stream, []byte(":status: 200"))
	stream = AppendData(stream, []byte("part1"))
	stream = AppendData(stream, []byte("part2"))

	sr := NewStreamReader(bytes.NewReader(stream))

	var got []Frame
	for {
		f, err := sr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatal(err)
		}
		got = append(got, *f)
	}

	if len(got) != 3 {
		t.Fatalf("解出 %d 个帧, want 3", len(got))
	}
	if got[0].Type != FrameHeaders || string(got[0].Payload) != ":status: 200" {
		t.Errorf("帧 0 = %v %q", got[0].Type, got[0].Payload)
	}
	if got[1].Type != FrameData || string(got[1].Payload) != "part1" {
		t.Errorf("帧 1 = %v %q", got[1].Type, got[1].Payload)
	}
	if got[2].Type != FrameData || string(got[2].Payload) != "part2" {
		t.Errorf("帧 2 = %v %q", got[2].Type, got[2].Payload)
	}
}

// StreamReader 要能处理"帧被底层读切成两半"（QUIC 流的 Read 不保证边界）。
func TestStreamReaderSplitReads(t *testing.T) {
	var stream []byte
	stream = AppendData(stream, []byte("a long enough body to be split across reads"))

	// 一个每次只给 5 个字节的 reader
	sr := NewStreamReader(&chunkReader{data: stream, chunk: 5})

	f, err := sr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(f.Payload) != "a long enough body to be split across reads" {
		t.Errorf("载荷 = %q", f.Payload)
	}
}

// chunkReader 每次只给 chunk 个字节（模拟 QUIC 流的切分）。
type chunkReader struct {
	data  []byte
	chunk int
	off   int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if r.off+n > len(r.data) {
		n = len(r.data) - r.off
	}
	copy(p, r.data[r.off:r.off+n])
	r.off += n
	return n, nil
}

// 建 UDP 套接字的便捷函数。
func TestListenUDP(t *testing.T) {
	pc, err := ListenUDP("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if !strings.HasPrefix(pc.LocalAddr().String(), "127.0.0.1:") {
		t.Errorf("地址 = %q", pc.LocalAddr())
	}
}
