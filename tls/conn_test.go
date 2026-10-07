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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"
)

// selfSigned 造一张自签证书给测试用。
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fio-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"fio-test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// pump 在两条连接之间搬密文。
//
// 这就是事件循环在现实里干的事：一边吐出来的密文喂给另一边。测试里用
// 一个循环 + 轮询代替 epoll。握手本身跑在各自的 goroutine 里。
func pump(t *testing.T, client, server *Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out := client.Take(); len(out) > 0 {
			server.Feed(out)
		}
		if out := server.Take(); len(out) > 0 {
			client.Feed(out)
		}
		if client.HandshakeDone() && server.HandshakeDone() {
			// 握手最后的输出也要送出去
			if out := client.Take(); len(out) > 0 {
				server.Feed(out)
			}
			if out := server.Take(); len(out) > 0 {
				client.Feed(out)
			}
			if err := client.HandshakeError(); err != nil {
				t.Fatalf("客户端握手失败: %v", err)
			}
			if err := server.HandshakeError(); err != nil {
				t.Fatalf("服务端握手失败: %v", err)
			}
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
	t.Fatal("握手没在 5 秒内完成")
}

// 握手要能走完，而且全程不阻塞——每次 Process 都是"给一点数据、拿一点
// 数据"，没有等待。
func TestHandshake(t *testing.T) {
	cert := selfSigned(t)
	client := Client(&tls.Config{InsecureSkipVerify: true})
	server := Server(&tls.Config{Certificates: []tls.Certificate{cert}})

	pump(t, client, server)

	if !client.HandshakeDone() {
		t.Fatal("客户端握手没完成")
	}
	if !server.HandshakeDone() {
		t.Fatal("服务端握手没完成")
	}

	st := server.ConnectionState()
	if st.Version == 0 {
		t.Error("协商出来的 TLS 版本是 0")
	}
	if st.CipherSuite == 0 {
		t.Error("协商出来的密码套件是 0")
	}
	t.Logf("TLS 版本=%#x 套件=%#x", st.Version, st.CipherSuite)
}

// 握手之后传数据：明文进、明文出，中间是密文。
func TestDataTransfer(t *testing.T) {
	cert := selfSigned(t)
	client := Client(&tls.Config{InsecureSkipVerify: true})
	server := Server(&tls.Config{Certificates: []tls.Certificate{cert}})
	pump(t, client, server)

	// 客户端发、服务端收
	msg := []byte("hello over tls")
	ct, err := client.Write(msg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ct, msg) {
		t.Fatal("明文原样出现在密文里——没有加密")
	}
	server.Feed(ct)

	var got []byte
	for i := 0; i < 100; i++ {
		got = server.Read()
		if len(got) > 0 {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	if string(got) != string(msg) {
		t.Fatalf("服务端收到 %q, want %q", got, msg)
	}

	// 服务端回、客户端收
	reply := []byte("and back")
	ct2, err := server.Write(reply)
	if err != nil {
		t.Fatal(err)
	}
	client.Feed(ct2)

	var gotReply []byte
	for i := 0; i < 100; i++ {
		gotReply = client.Read()
		if len(gotReply) > 0 {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	if string(gotReply) != string(reply) {
		t.Fatalf("客户端收到 %q, want %q", gotReply, reply)
	}
}

// 数据被切成一个字节一个字节地喂，TLS 也得能处理——这是非阻塞 io 的
// 常态（TCP 会在任意位置切）。
func TestByteByByteHandshake(t *testing.T) {
	cert := selfSigned(t)
	client := Client(&tls.Config{InsecureSkipVerify: true})
	server := Server(&tls.Config{Certificates: []tls.Certificate{cert}})

	// 一个字节一个字节地搬
	var c2sPending, s2cPending []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c2sPending = append(c2sPending, client.Take()...)
		s2cPending = append(s2cPending, server.Take()...)

		if len(c2sPending) > 0 {
			server.Feed(c2sPending[:1])
			c2sPending = c2sPending[1:]
		}
		if len(s2cPending) > 0 {
			client.Feed(s2cPending[:1])
			s2cPending = s2cPending[1:]
		}
		if client.HandshakeDone() && server.HandshakeDone() &&
			len(c2sPending) == 0 && len(s2cPending) == 0 {
			break
		}
		time.Sleep(20 * time.Microsecond)
	}

	if !client.HandshakeDone() || !server.HandshakeDone() {
		t.Fatal("逐字节喂的时候握手没完成")
	}
	if err := client.HandshakeError(); err != nil {
		t.Fatalf("客户端握手失败: %v", err)
	}
	if err := server.HandshakeError(); err != nil {
		t.Fatalf("服务端握手失败: %v", err)
	}
}

// 握手没完成时 Write 要返回 ErrWantMore，不能悄悄把明文发出去。
func TestWriteBeforeHandshake(t *testing.T) {
	client := Client(&tls.Config{InsecureSkipVerify: true})
	if _, err := client.Write([]byte("too early")); !errors.Is(err, ErrHandshakeIncomplete) {
		t.Fatalf("握手前 Write 返回 %v, want ErrHandshakeIncomplete", err)
	}
}

// 没有数据时握手要挂在那儿等（不是失败、不是空转烧 CPU）。
func TestNoDataYet(t *testing.T) {
	cert := selfSigned(t)
	server := Server(&tls.Config{Certificates: []tls.Certificate{cert}})
	if server.Take() != nil {
		t.Fatal("还没喂数据就有输出")
	}
	// 给它 100ms 看会不会自己完成（它应该一直等着 Feed）
	time.Sleep(100 * time.Millisecond)
	if server.HandshakeDone() {
		t.Fatal("没喂数据却握手完成了")
	}
	if server.HandshakeError() != nil {
		t.Fatal("没喂数据不该是错误")
	}
}

// 丢进去垃圾数据，握手要失败（而不是卡住或 panic）。
func TestBadHandshake(t *testing.T) {
	cert := selfSigned(t)
	server := Server(&tls.Config{Certificates: []tls.Certificate{cert}})

	// 一大坨不是 TLS 记录的东西
	junk := bytes.Repeat([]byte{0xde, 0xad, 0xbe, 0xef}, 64)
	server.Feed(junk)

	if err := server.WaitHandshake(); err == nil {
		t.Fatal("垃圾数据没有让握手失败")
	}
	if server.HandshakeError() == nil {
		t.Fatal("握手应该记下失败原因")
	}
	// 失败之后不能变成成功
	time.Sleep(10 * time.Millisecond)
	if server.HandshakeError() == nil {
		t.Fatal("失败状态不该自己变好")
	}
}

// 大消息（超过一个 TLS 记录）也要能完整传过去。
func TestLargeMessage(t *testing.T) {
	cert := selfSigned(t)
	client := Client(&tls.Config{InsecureSkipVerify: true})
	server := Server(&tls.Config{Certificates: []tls.Certificate{cert}})
	pump(t, client, server)

	// 64KB，比一个 TLS 记录（16KB）大
	msg := make([]byte, 64*1024)
	for i := range msg {
		msg[i] = byte(i * 31)
	}
	ct, err := client.Write(msg)
	if err != nil {
		t.Fatal(err)
	}
	server.Feed(ct)

	var got []byte
	for i := 0; i < 200; i++ {
		got = append(got, server.Read()...)
		if len(got) >= len(msg) {
			break
		}
		time.Sleep(50 * time.Microsecond)
	}
	if len(got) != len(msg) {
		t.Fatalf("收到 %d 字节, want %d", len(got), len(msg))
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("大消息内容不一致")
	}
}
