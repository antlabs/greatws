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

package tls

import (
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/antlabs/fio/engine"
	"golang.org/x/sys/unix"
)

// startTLSServer 起一个真实的 TLS 服务端（engine 的 epoll 循环 + TLS 状态机）。
//
// inner 是 TLS 里面的协议（可以为 nil，那就只测握手）。
func startTLSServer(t *testing.T, cfg *Config, inner engine.Handler) (string, func()) {
	t.Helper()

	m, err := engine.NewAndStart(engine.WithEventLoops(2), engine.WithLogLevel(slog.LevelError))
	if err != nil {
		t.Fatal(err)
	}

	// accept 循环交给 engine.Listener：非阻塞 accept + 停止标志，
	// 两个平台的 Close 行为一致（见那个类型的说明）。
	ln, err := engine.ListenAndServe(m, "127.0.0.1:0", func() engine.Handler {
		return NewConnHandler(cfg, inner)
	})
	if err != nil {
		t.Fatal(err)
	}

	stop := func() {
		ln.Close()
		m.Free()
	}
	return ln.Addr(), stop
}

// echoInner 是 TLS 里面的测试协议：把收到的明文原样发回去。
type echoInner struct{}

func (echoInner) OnOpen(*engine.Conn) {}

func (echoInner) OnData(c *engine.Conn, buf []byte) (int, error) {
	// c.Write 会进写拦截器、被加密（这就是测的东西）
	if err := c.Write(buf); err != nil {
		return 0, err
	}
	return len(buf), nil
}

func (echoInner) OnClose(*engine.Conn, error) {}

// **端到端**：TLS 服务端跑在 engine 上，客户端用**标准库的 crypto/tls**。
//
// 这条路径证明：TLS 状态机真的接上事件循环了——fd 读到密文、状态机解密、
// 明文给内层、内层的输出加密写回 fd。而且对端是标准库（不是自己），
// 说明协议实现是对的。
func TestEngineTLSWithStdlibClient(t *testing.T) {
	cert := selfSigned(t)
	addr, stop := startTLSServer(t, &Config{Certificates: []tls.Certificate{cert}}, echoInner{})
	defer stop()

	// 标准库的 TLS 客户端
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(10 * time.Second))

	cli := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, ServerName: "fio-test"})
	if err := cli.Handshake(); err != nil {
		t.Fatalf("标准库客户端握手失败: %v", err)
	}
	t.Logf("握手成功，套件=%#x 版本=%#x", cli.ConnectionState().CipherSuite, cli.ConnectionState().Version)

	// 发明文，收回来
	msg := []byte("hello over state-machine tls")
	if _, err := cli.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(cli, got); err != nil {
		t.Fatalf("读回显: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("回显 = %q, want %q", got, msg)
	}
}

// 多次来回（验证记录层的序列号、密钥这些都持续正确）。
func TestEngineTLSMultipleRoundTrips(t *testing.T) {
	cert := selfSigned(t)
	addr, stop := startTLSServer(t, &Config{Certificates: []tls.Certificate{cert}}, echoInner{})
	defer stop()

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(10 * time.Second))

	cli := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, ServerName: "fio-test"})
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 20; i++ {
		msg := []byte(fmt.Sprintf("message %d", i))
		if _, err := cli.Write(msg); err != nil {
			t.Fatalf("第 %d 次写: %v", i, err)
		}
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(cli, got); err != nil {
			t.Fatalf("第 %d 次读: %v", i, err)
		}
		if string(got) != string(msg) {
			t.Fatalf("第 %d 次: %q != %q", i, got, msg)
		}
	}
}

// 大消息（跨多条 TLS 记录）。
func TestEngineTLSLargeMessage(t *testing.T) {
	cert := selfSigned(t)
	addr, stop := startTLSServer(t, &Config{Certificates: []tls.Certificate{cert}}, echoInner{})
	defer stop()

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(15 * time.Second))

	cli := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, ServerName: "fio-test"})
	if err := cli.Handshake(); err != nil {
		t.Fatal(err)
	}

	// 64KB：超过一条 TLS 记录的载荷上限（16KB）
	msg := make([]byte, 64*1024)
	for i := range msg {
		msg[i] = byte(i * 17)
	}
	go func() {
		cli.Write(msg)
	}()

	got := make([]byte, len(msg))
	if _, err := io.ReadFull(cli, got); err != nil {
		t.Fatalf("读大消息: %v", err)
	}
	for i := range msg {
		if got[i] != msg[i] {
			t.Fatalf("第 %d 字节不同: %02x != %02x", i, got[i], msg[i])
		}
	}
}

// 我们的客户端 和 标准库的服务端（反向）。
func TestEngineTLSOurClientStdlibServer(t *testing.T) {
	cert := selfSigned(t)

	// 标准库服务端
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srvDone := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			srvDone <- err
			return
		}
		defer conn.Close()
		srv := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		if err := srv.Handshake(); err != nil {
			srvDone <- err
			return
		}
		buf := make([]byte, 256)
		n, err := srv.Read(buf)
		if err != nil {
			srvDone <- err
			return
		}
		if _, err := srv.Write(buf[:n]); err != nil {
			srvDone <- err
			return
		}
		srvDone <- nil
	}()

	// 我们的客户端（跑在 engine 上）
	m, err := engine.NewAndStart(engine.WithEventLoops(1), engine.WithLogLevel(slog.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Free()

	// 拨号 + 设非阻塞
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	sa := &unix.SockaddrInet4{Port: port}
	copy(sa.Addr[:], net.ParseIP(host).To4())
	if err := unix.Connect(fd, sa); err != nil {
		t.Fatal(err)
	}
	unix.SetNonblock(fd, true)

	inner := &clientRecorder{echo: make(chan []byte, 1)}
	if _, err := m.Add(fd, NewClientConnHandler(&Config{
		InsecureSkipVerify: true,
		ServerName:         "fio-test",
	}, inner)); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-srvDone:
		if err != nil {
			t.Fatalf("标准库服务端: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时")
	}

	// 服务端是"读什么回什么"，回显要能一路解密到内层协议
	select {
	case got := <-inner.echo:
		if string(got) != clientPayload {
			t.Fatalf("回显 = %q, want %q", got, clientPayload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("内层协议没收到回显")
	}
}

const clientPayload = "ping over engine tls"

// clientRecorder 客户端的内层协议：握手完成就发一条明文，
// 收到回显就记下来。
//
// **它在 OnOpen 里写**——那正是 TLS 适配层通知"握手完了"的时刻，
// 从那一刻起 c.Write 会走写拦截器（被加密）。这条路径就是
// "内层协议在事件循环上说话"的完整验证。
type clientRecorder struct {
	echo chan []byte
}

func (c *clientRecorder) OnOpen(conn *engine.Conn) {
	conn.Write([]byte(clientPayload))
}

func (c *clientRecorder) OnData(_ *engine.Conn, b []byte) (int, error) {
	select {
	case c.echo <- append([]byte(nil), b...):
	default:
	}
	return len(b), nil
}

func (c *clientRecorder) OnClose(*engine.Conn, error) {}
