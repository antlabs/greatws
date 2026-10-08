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

//go:build linux || darwin

// 跨实现的正确性测试：拿**别人的**客户端打我们的服务端。
//
// 为什么要有：自己写的测试只能验证"我以为要想的那些点"。真正能说明
// 问题的是——标准库的 http.Client 这种**别人实现的、被广泛使用的**
// 客户端，能不能跟我们的服务端正常对话。它对协议的实现是独立的，
// 不共享我们的任何假设，所以能打到我们自己想不到的地方。
//
// （这个文件放在 http 包里而不是仓库根目录：放根目录的话它会 import
// 自己的子包，而这个仓库被别人 replace 成别的路径时，那种自引用就
// 解析不了——go mod tidy 会跑去网上找 github.com/antlabs/fio/http。）
package http

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antlabs/fio/engine"
	"golang.org/x/sys/unix"
)

// startHTTPServer 起一个 fio 的 HTTP/1.1 服务端，返回地址。
func startHTTPServer(t *testing.T, h Handler) (string, func()) {
	t.Helper()

	m, err := engine.NewAndStart(engine.WithEventLoops(2))
	if err != nil {
		t.Fatal(err)
	}

	lfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	unix.SetsockoptInt(lfd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
	sa := &unix.SockaddrInet4{}
	copy(sa.Addr[:], []byte{127, 0, 0, 1})
	if err := unix.Bind(lfd, sa); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(lfd, 128); err != nil {
		t.Fatal(err)
	}
	bound, _ := unix.Getsockname(lfd)
	port := bound.(*unix.SockaddrInet4).Port

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			nfd, _, err := unix.Accept(lfd)
			if err != nil {
				return
			}
			unix.SetNonblock(nfd, true)
			if _, err := m.Add(nfd, NewConnHandler(h, 0)); err != nil {
				unix.Close(nfd)
			}
		}
	}()

	stop := func() {
		unix.Close(lfd)
		<-done
		m.Free()
	}
	return fmt.Sprintf("127.0.0.1:%d", port), stop
}

// **标准库的 http.Client 打我们的服务端**。
//
// 这是最要紧的一条互操作测试：net/http 是全世界上用得最多的 HTTP 客户端，
// 它对报文的构造和解析是独立的实现。它能正常拿到响应，说明我们的响应
// 格式是对的。
func TestStdlibClientAgainstOurServer(t *testing.T) {
	var mu sync.Mutex
	var paths []string

	addr, stop := startHTTPServer(t, HandlerFunc(func(w *ResponseWriter, r *Request) {
		mu.Lock()
		paths = append(paths, r.Target)
		mu.Unlock()

		w.Header()["Content-Type"] = []string{"text/plain"}
		body := "you asked for " + r.Target
		w.Header()["Content-Length"] = []string{fmt.Sprint(len(body))}
		w.Write([]byte(body))
	}))
	defer stop()

	client := &http.Client{Timeout: 5 * time.Second}

	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("/item%d", i)
		resp, err := client.Get("http://" + addr + path)
		if err != nil {
			t.Fatalf("第 %d 个请求: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("状态码 = %d, want 200", resp.StatusCode)
		}
		want := "you asked for " + path
		if string(body) != want {
			t.Errorf("响应体 = %q, want %q", body, want)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/plain" {
			t.Errorf("Content-Type = %q", ct)
		}
	}

	// 标准库默认 keep-alive，所以五个请求应该复用连接（但也可能重连，
	// 所以只校验路径顺序）
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 5 {
		t.Fatalf("服务端收到 %d 个请求, want 5: %v", len(paths), paths)
	}
	for i := range paths {
		if paths[i] != fmt.Sprintf("/item%d", i) {
			t.Errorf("#%d = %q", i, paths[i])
		}
	}
}

// 标准库的 POST（带 body）+ 我们的服务端。
func TestStdlibPostAgainstOurServer(t *testing.T) {
	type got struct{ method, path, body, ctype string }
	gotCh := make(chan got, 1)

	addr, stop := startHTTPServer(t, HandlerFunc(func(w *ResponseWriter, r *Request) {
		ct, _ := r.Get("Content-Type")
		select {
		case gotCh <- got{r.Method, r.Target, string(r.Body), ct}:
		default:
		}
		w.Header()["Content-Length"] = []string{"2"}
		w.Write([]byte("ok"))
	}))
	defer stop()

	client := &http.Client{Timeout: 5 * time.Second}
	body := "this is the request body"
	resp, err := client.Post("http://"+addr+"/submit", "text/plain", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 || string(respBody) != "ok" {
		t.Errorf("响应 = %d %q", resp.StatusCode, respBody)
	}

	select {
	case g := <-gotCh:
		if g.method != "POST" {
			t.Errorf("方法 = %q, want POST", g.method)
		}
		if g.path != "/submit" {
			t.Errorf("路径 = %q", g.path)
		}
		if g.body != body {
			t.Errorf("body = %q, want %q", g.body, body)
		}
		if g.ctype != "text/plain" {
			t.Errorf("Content-Type = %q", g.ctype)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没收到请求")
	}
}

// 标准库发各种头，我们要原样认出来（头名大小写、多个值）。
func TestStdlibHeaders(t *testing.T) {
	type hdrs struct {
		a, b, multi string
	}
	gotCh := make(chan hdrs, 1)

	addr, stop := startHTTPServer(t, HandlerFunc(func(w *ResponseWriter, r *Request) {
		a, _ := r.Get("X-Custom-A")
		b, _ := r.Get("x-custom-b") // 小写查
		m, _ := r.Get("X-Multi")
		select {
		case gotCh <- hdrs{a, b, m}:
		default:
		}
		w.Header()["Content-Length"] = []string{"1"}
		w.Write([]byte("x"))
	}))
	defer stop()

	req, _ := http.NewRequest("GET", "http://"+addr+"/", nil)
	req.Header.Set("X-Custom-A", "value-a")
	req.Header.Set("X-Custom-B", "value-b")
	req.Header.Add("X-Multi", "one")
	req.Header.Add("X-Multi", "two")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	select {
	case g := <-gotCh:
		if g.a != "value-a" {
			t.Errorf("X-Custom-A = %q", g.a)
		}
		if g.b != "value-b" {
			t.Errorf("X-Custom-B = %q（小写查询要能查到）", g.b)
		}
		if !strings.Contains(g.multi, "one") || !strings.Contains(g.multi, "two") {
			t.Errorf("X-Multi = %q, 两个值都该在", g.multi)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("服务端没收到请求")
	}
}

// 大 body 要能完整收到——**这是抓出过一个死锁级 bug 的测试**。
//
// 早先引擎的读缓冲区长到 16KB 就不再长了（那边只考虑了"一次读能装下
// 一个批次"，没想到"协议还没消费完、缓冲区就满了"这条路）。症状：
// 32KB 的 POST 永久卡住——缓冲区填满、解析器等剩下的数据、又没空间读
// 新的，Read 每次都回 0 字节。
//
// 自己写的测试撞不到（那些 body 都在一次 read 里装得下），是**标准库的
// http.Client 发 64KB POST** 才打出来的。所以这个测试跨了多个尺寸，
// 把边界两侧都覆盖上。
func TestLargeBody(t *testing.T) {
	gotCh := make(chan int, 8)

	addr, stop := startHTTPServer(t, HandlerFunc(func(w *ResponseWriter, r *Request) {
		select {
		case gotCh <- len(r.Body):
		default:
		}
		w.Header()["Content-Length"] = []string{"2"}
		w.Write([]byte("ok"))
	}))
	defer stop()

	// 这几个尺寸刻意跨过 16KB（batchReadBufferSize）——那正是死锁的边界
	for _, size := range []int{1000, 16 * 1024, 32 * 1024, 64 * 1024, 512 * 1024} {
		body := strings.Repeat("x", size)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Post("http://"+addr+"/big", "application/octet-stream", strings.NewReader(body))
		if err != nil {
			t.Fatalf("size=%d: %v", size, err)
		}
		rb, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("size=%d: 读响应: %v", size, err)
		}
		if string(rb) != "ok" {
			t.Fatalf("size=%d: 响应 = %q", size, rb)
		}

		select {
		case n := <-gotCh:
			if n != size {
				t.Errorf("size=%d: 服务端收到 %d 字节", size, n)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("size=%d: 服务端没收到请求", size)
		}
	}
}

// 我们用标准库的 httptest 当**服务端**，自己写客户端去打它 ——
// 验证我们读响应的能力（这个方向目前只有简单实现，先跳过）
func TestOurClientAgainstStdlibServer(t *testing.T) {
	t.Skip("fio 还没有 HTTP 客户端实现（http 包只有服务端）")
}

// 多个客户端并发打（标准库的 Transport 会开多条连接）。
func TestConcurrentClients(t *testing.T) {
	addr, stop := startHTTPServer(t, HandlerFunc(func(w *ResponseWriter, r *Request) {
		body := "ok:" + r.Target
		w.Header()["Content-Length"] = []string{fmt.Sprint(len(body))}
		w.Write([]byte(body))
	}))
	defer stop()

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 20},
	}

	const n = 50
	var wg sync.WaitGroup
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			path := fmt.Sprintf("/c%d", id)
			resp, err := client.Get("http://" + addr + path)
			if err != nil {
				errCh <- err
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if want := "ok:" + path; string(body) != want {
				errCh <- fmt.Errorf("响应 = %q, want %q", body, want)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

// TLS：用标准库的 tls.Client 打我们的 TLS 服务端（用 fio 的 tls 包）。
func TestStdlibTLSClient(t *testing.T) {
	// 这条需要 fio 的 tls 包和 engine 组合起来用，现在还没接
	// （tls 包是独立的，接进 engine 是下一步）
	t.Skip("fio 的 tls 包还没和 engine 接起来")
	if false {
		_ = tls.Config{}
	}
	_ = net.Dial
}
