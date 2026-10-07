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

package engine

import (
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// echoHandler 是给测试用的协议：收到什么原样写回去。
//
// 这也是 Handler 接口该有的样子——协议的实现者只要管"这些字节什么意思"，
// epoll 注册、非阻塞读写、部分写、EAGAIN 全是引擎的事。
type echoHandler struct{}

func (echoHandler) OnOpen(*Conn) {}

func (echoHandler) OnData(c *Conn, buf []byte) (int, error) {
	n := len(buf)
	if err := c.Write(buf); err != nil {
		return 0, err
	}
	return n, nil
}

func (echoHandler) OnClose(*Conn, error) {}

// lineHandler 每次只吃一行，测试"协议说还不够"这条路。
type lineHandler struct {
	mu    sync.Mutex
	lines []string
	done  chan struct{}
}

func (h *lineHandler) OnOpen(*Conn) {}

func (h *lineHandler) OnData(c *Conn, buf []byte) (int, error) {
	// 找一行
	for i, b := range buf {
		if b != '\n' {
			continue
		}
		line := string(buf[:i])
		h.mu.Lock()
		h.lines = append(h.lines, line)
		closed := len(h.lines) >= 3
		h.mu.Unlock()
		if closed && h.done != nil {
			select {
			case <-h.done:
			default:
				close(h.done)
			}
		}
		// 吃掉这一行（含换行），剩下的留给下一次
		return i + 1, nil
	}
	// 还没有换行，一行都没凑齐
	return 0, nil
}

func (h *lineHandler) OnClose(*Conn, error) {}

// startServer 起一个监听 fd，用 engine 的循环 accept 它。
//
// 返回监听地址和一个关停函数。
func startServer(t *testing.T, h Handler) (string, func()) {
	t.Helper()

	m, err := NewAndStart(WithEventLoops(1), WithLogLevel(slog.LevelError))
	if err != nil {
		t.Fatal(err)
	}

	// 监听 socket
	// 监听 fd 用阻塞模式: accept 阻塞在内核里等连接, 有连接立刻被唤醒,
	// 不用自旋。新连接再设成非阻塞交给引擎。
	lfd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.SetsockoptInt(lfd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		t.Fatal(err)
	}
	sa := &unix.SockaddrInet4{Port: 0}
	copy(sa.Addr[:], []byte{127, 0, 0, 1})
	if err := unix.Bind(lfd, sa); err != nil {
		t.Fatal(err)
	}
	if err := unix.Listen(lfd, 128); err != nil {
		t.Fatal(err)
	}
	bound, err := unix.Getsockname(lfd)
	if err != nil {
		t.Fatal(err)
	}
	port := bound.(*unix.SockaddrInet4).Port

	// accept 循环。阻塞 accept, 关掉 lfd 它就返回错误退出——生产里
	// accept 该挂到事件循环上（那是 TcpServer 的事）。
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			nfd, _, err := unix.Accept(lfd)
			if err != nil {
				return
			}
			if err := unix.SetNonblock(nfd, true); err != nil {
				unix.Close(nfd)
				continue
			}
			if _, err := m.Add(nfd, h); err != nil {
				unix.Close(nfd)
			}
		}
	}()

	stop := func() {
		unix.Close(lfd) // 让 accept 返回
		<-acceptDone    // 等 accept 循环真的退出，再 Free
		m.Free()
	}
	return "127.0.0.1:" + itoa(port), stop
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestEcho 最基本的一条：连上、发数据、原样收回来。
func TestEcho(t *testing.T) {
	addr, stop := startServer(t, echoHandler{})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	msg := []byte("hello engine")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(msg) {
		t.Errorf("got %q, want %q", got, msg)
	}
}

// 一次写多条，服务端一次 read 可能全收到——echo 要原样回来，不能串。
func TestEchoBatch(t *testing.T) {
	addr, stop := startServer(t, echoHandler{})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	const n = 50
	const each = 100
	var want []byte
	for i := 0; i < n; i++ {
		chunk := make([]byte, each)
		for j := range chunk {
			chunk[j] = byte('a' + i%26)
		}
		want = append(want, chunk...)
	}
	if _, err := conn.Write(want); err != nil {
		t.Fatal(err)
	}

	got := make([]byte, len(want))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个字节: got %q, want %q", i, got[i], want[i])
		}
	}
}

// 协议说"还不够"时数据要留着：一行一行地喂，每行到齐才回调一次。
//
// 这是 Handler 契约的核心——OnData 返回 0 不是错误，是"再给我点"。
func TestPartialMessage(t *testing.T) {
	h := &lineHandler{done: make(chan struct{})}
	addr, stop := startServer(t, h)
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 一次只发一个字节，中间还有停顿
	lines := []string{"aaa", "bbb", "ccc"}
	for _, ln := range lines {
		for i := 0; i < len(ln); i++ {
			if _, err := conn.Write([]byte{ln[i]}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
		if _, err := conn.Write([]byte{'\n'}); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.mu.Lock()
		t.Fatalf("超时, 收到 %v", h.lines)
		h.mu.Unlock()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines) != len(lines) {
		t.Fatalf("收到 %d 行, want %d: %v", len(h.lines), len(lines), h.lines)
	}
	for i := range lines {
		if h.lines[i] != lines[i] {
			t.Errorf("第 %d 行 = %q, want %q", i, h.lines[i], lines[i])
		}
	}
}

// 多条连接同时在。它们按 fd 分片到同一个循环上，互不干扰。
func TestManyConns(t *testing.T) {
	addr, stop := startServer(t, echoHandler{})
	defer stop()

	const conns = 20
	var wg sync.WaitGroup
	errCh := make(chan error, conns)

	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()

			msg := []byte{byte('A' + id%26), byte('0' + id%10), 'x', 'y', 'z'}
			if _, err := conn.Write(msg); err != nil {
				errCh <- err
				return
			}
			got := make([]byte, len(msg))
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(conn, got); err != nil {
				errCh <- err
				return
			}
			if string(got) != string(msg) {
				errCh <- io.ErrUnexpectedEOF
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

// 对端关了以后，OnClose 要调到（连接要能回收）。
func TestClose(t *testing.T) {
	closed := make(chan struct{}, 1)
	h := &closeHandler{onClose: func() {
		select {
		case closed <- struct{}{}:
		default:
		}
	}}
	addr, stop := startServer(t, h)
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	// 发点东西确认连上了，再关
	conn.Write([]byte("hi"))
	time.Sleep(50 * time.Millisecond)
	conn.Close()

	// 对端关掉之后，服务端下一次读会读到 EOF。往下要主动去读——ET 模式
	// 下 FIN 会来一个可读事件，引擎会读到 0。这里给它个机会。
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("5 秒内没等到 OnClose")
	}
}

type closeHandler struct {
	onClose func()
}

func (h *closeHandler) OnOpen(*Conn) {}

func (h *closeHandler) OnData(c *Conn, buf []byte) (int, error) {
	// 读到 0 字节说明是 FIN
	if len(buf) == 0 {
		return 0, io.EOF
	}
	return len(buf), nil
}

func (h *closeHandler) OnClose(*Conn, error) {
	if h.onClose != nil {
		h.onClose()
	}
}
