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
	"bytes"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"
)

// pumpState 在两条状态机之间搬密文，直到握手完成。
//
// 这就是事件循环在现实里干的事：一边吐出来的密文喂给另一边。
func pumpState(t *testing.T, client, server *StateMachine, byteByByte bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var c2s, s2c []byte

	for time.Now().Before(deadline) {
		c2s = append(c2s, client.TakeOutput()...)
		s2c = append(s2c, server.TakeOutput()...)

		// 喂：可以整块喂，也可以逐字节（两种都要能跑）
		if byteByByte {
			if len(c2s) > 0 {
				server.Feed(c2s[:1])
				c2s = c2s[1:]
			}
			if len(s2c) > 0 {
				client.Feed(s2c[:1])
				s2c = s2c[1:]
			}
		} else {
			if len(c2s) > 0 {
				server.Feed(c2s)
				c2s = nil
			}
			if len(s2c) > 0 {
				client.Feed(s2c)
				s2c = nil
			}
		}

		if client.Established() && server.Established() {
			return
		}
		if client.Error() != nil {
			t.Fatalf("客户端握手失败: %v", client.Error())
		}
		if server.Error() != nil {
			t.Fatalf("服务端握手失败: %v", server.Error())
		}
		if len(c2s) == 0 && len(s2c) == 0 && !byteByByte {
			// 没有数据可搬了，也没有进展——可能是死锁
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatalf("握手没完成: client=%v server=%v",
		client.State(), server.State())
}

// **自己跟自己握手**：两条状态机对喂，握手要能走完。
func TestStateMachineHandshake(t *testing.T) {
	cert := selfSigned(t)

	server := NewServerStateMachine(&tls.Config{Certificates: []tls.Certificate{cert}})
	client := NewClientStateMachine(&tls.Config{InsecureSkipVerify: true})

	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	pumpState(t, client, server, false)

	if client.CipherSuite() != TLS_AES_128_GCM_SHA256 {
		t.Errorf("客户端协商的套件 = %#x", client.CipherSuite())
	}
	if server.CipherSuite() != TLS_AES_128_GCM_SHA256 {
		t.Errorf("服务端协商的套件 = %#x", server.CipherSuite())
	}
	if server.ClientHello() == nil {
		t.Error("服务端没记录 ClientHello")
	}
}

// 逐字节喂（TCP 会在任意位置切）也要能握手。
func TestStateMachineByteByByte(t *testing.T) {
	cert := selfSigned(t)

	server := NewServerStateMachine(&tls.Config{Certificates: []tls.Certificate{cert}})
	client := NewClientStateMachine(&tls.Config{InsecureSkipVerify: true})

	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	pumpState(t, client, server, true)
}

// 握手之后传应用数据。
func TestStateMachineData(t *testing.T) {
	cert := selfSigned(t)

	server := NewServerStateMachine(&tls.Config{Certificates: []tls.Certificate{cert}})
	client := NewClientStateMachine(&tls.Config{InsecureSkipVerify: true})
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	pumpState(t, client, server, false)

	// 客户端发
	msg := []byte("hello from the state machine")
	ct, err := client.Write(msg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, msg) {
		t.Fatal("明文出现在密文里")
	}
	if err := server.Feed(ct); err != nil {
		t.Fatal(err)
	}
	if got := server.ReadPlaintext(); !bytes.Equal(got, msg) {
		t.Fatalf("服务端收到 %q, want %q", got, msg)
	}

	// 服务端回
	reply := []byte("and hello back")
	ct2, err := server.Write(reply)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Feed(ct2); err != nil {
		t.Fatal(err)
	}
	if got := client.ReadPlaintext(); !bytes.Equal(got, reply) {
		t.Fatalf("客户端收到 %q, want %q", got, reply)
	}
}

// 大消息（跨多条记录）要能完整传。
func TestStateMachineLargeData(t *testing.T) {
	cert := selfSigned(t)

	server := NewServerStateMachine(&tls.Config{Certificates: []tls.Certificate{cert}})
	client := NewClientStateMachine(&tls.Config{InsecureSkipVerify: true})
	if err := client.Start(); err != nil {
		t.Fatal(err)
	}
	pumpState(t, client, server, false)

	msg := bytes.Repeat([]byte("abcdefgh"), 8192) // 64KB
	ct, err := client.Write(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.Feed(ct); err != nil {
		t.Fatal(err)
	}
	got := server.ReadPlaintext()
	if !bytes.Equal(got, msg) {
		t.Fatalf("收到 %d 字节, want %d", len(got), len(msg))
	}
}

// 握手没完成时 Write 要报错，不能悄悄发明文。
func TestStateMachineWriteBeforeHandshake(t *testing.T) {
	cert := selfSigned(t)
	server := NewServerStateMachine(&tls.Config{Certificates: []tls.Certificate{cert}})
	if _, err := server.Write([]byte("too early")); !errors.Is(err, ErrHandshakeIncomplete) {
		t.Fatalf("err = %v, want ErrHandshakeIncomplete", err)
	}
}

// 垃圾数据要握手失败，不能卡住。
func TestStateMachineBadData(t *testing.T) {
	cert := selfSigned(t)
	server := NewServerStateMachine(&tls.Config{Certificates: []tls.Certificate{cert}})

	junk := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 64)
	server.Feed(junk)

	// 不该 panic，状态要么没完成要么失败
	if server.Established() {
		t.Fatal("垃圾数据之后握手居然完成了")
	}
}

// ---- 跟标准库的 crypto/tls 握手（**这是最要紧的**）----

// **我们的客户端状态机 和 标准库的服务端 握手**。
//
// **这个测试现在是失败的**，留着是因为它指出了下一步要查什么：标准库
// 在解析我们的 ClientHello 阶段就回了 fatal handshake_failure(40)，
// 而自己的状态机互相握手是好的。说明我们的 ClientHello 里有标准库
// 不接受的东西——逐字节对比标准库自己发的 ClientHello 就能找出来。
//
// 不删它的理由：这正是跨实现验证的价值所在。自己跟自己握手全绿，
// 但真实实现不认——这种事只有拿对方的实现来打才能发现。
//
// 用 net.Pipe 把两边接起来：标准库那头是阻塞的（跑在自己的 goroutine
// 里），我们这头是状态机（主 goroutine 里一步一步推）。能握上手说明
// 我们的 ClientHello、密钥派生、Finished 都对。
func TestInteropWithStdlibServer(t *testing.T) {
	cert := selfSigned(t)

	stdConfig := &tls.Config{Certificates: []tls.Certificate{cert}}
	stdServer := tls.Server(nil, stdConfig)

	ourClient := NewClientStateMachine(&tls.Config{InsecureSkipVerify: true})
	if err := ourClient.Start(); err != nil {
		t.Fatal(err)
	}

	// 用 channel 搬字节（标准库那头是阻塞的，得放 goroutine）
	toStd := make(chan []byte, 16)
	toUs := make(chan []byte, 16)

	// 把标准库的 Conn 换成基于 channel 的（用 net.Pipe 最方便）
	ourSide, stdSide := net.Pipe()
	stdServer = tls.Server(stdSide, stdConfig)

	// 标准库那头：跑完整握手
	stdErr := make(chan error, 1)
	go func() {
		stdErr <- stdServer.Handshake()
	}()

	// 我们这头：状态机推进
	done := make(chan error, 1)
	go func() {
		_ = toStd
		_ = toUs
		deadline := time.Now().Add(10 * time.Second)

		// 先把 ClientHello 发出去
		if out := ourClient.TakeOutput(); len(out) > 0 {
			if _, err := ourSide.Write(out); err != nil {
				done <- err
				return
			}
		}

		buf := make([]byte, 16*1024)
		for time.Now().Before(deadline) {
			n, err := ourSide.Read(buf)
			if err != nil {
				done <- err
				return
			}
			if n > 0 {
				if err := ourClient.Feed(buf[:n]); err != nil {
					done <- err
					return
				}
				if ourClient.Error() != nil {
					done <- ourClient.Error()
					return
				}
				if out := ourClient.TakeOutput(); len(out) > 0 {
					if _, err := ourSide.Write(out); err != nil {
						done <- err
						return
					}
				}
				if ourClient.Established() {
					done <- nil
					return
				}
			}
		}
		done <- errors.New("超时")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("我们的客户端状态机失败: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("超时")
	}

	select {
	case err := <-stdErr:
		if err != nil {
			t.Fatalf("标准库服务端握手失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("标准库那头没结束")
	}

	t.Logf("我们的状态机客户端 和 标准库服务端 握手成功，套件=%#x", ourClient.CipherSuite())
}
