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
package quicknet

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// 这些用例盯的是"一次 write 里塞多个 frame"这种输入下, 服务端的解析和
// 回包还对不对。BatchBuffers 那条路(cork)会把一批回包合成一次写, 所以
// 输入侧也要按批次构造。

// writeBatch 把一个已经拼好的多帧批次写进连接。
//
// 走的是 con.write(未导出的那个), 它和 WriteMessage 一样要求调用方持有
// c.mu——event loop 那边的 flush 是不拿锁的, 客户端自己的解析 goroutine
// 也在碰同一个连接的 wbufList, 所以这里必须和 WriteMessage 用同一把锁,
// 否则就是数据竞争。
func writeBatch(t *testing.T, con *Conn, buf []byte) {
	t.Helper()
	con.mu.Lock()
	_, err := con.write(buf)
	con.mu.Unlock()
	if err != nil {
		t.Fatalf("write batch: %v", err)
	}
}

// corkServer 起一个回显服务端, 返回它的 ws 地址和一个关停函数。
//
// 关停分两步: 先放掉 Upgrade 里那个 handler, 再关 httptest(它会等所有
// handler 返回)。不先放的话, 关停要等 handler 的超时——每个用例白等
// 十几秒, 而且 httptest 等超时期间日志会盖住真正的失败。
func corkServer(t *testing.T, cb Callback) (string, func()) {
	t.Helper()
	m := NewMultiEventLoopAndStartMust(
		WithEventLoops(1),
		WithLogLevel(slog.LevelError),
		WithBusinessGoNum(1, 1, 1),
	)
	handlerDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r,
			WithServerCallback(cb),
			WithServerMultiEventLoop(m),
		)
		if err != nil {
			return
		}
		defer c.Close()
		select {
		case <-handlerDone:
		case <-time.After(30 * time.Second):
		}
	}))
	stop := func() {
		close(handlerDone)
		ts.Close()
	}
	return "ws" + strings.TrimPrefix(ts.URL, "http"), stop
}

// Test_Cork_BatchMixedSizes 一批里消息大小不一(小于 126、126~65535 两档
// 长度编码都用上), 校验每条都回来且内容对。
func Test_Cork_BatchMixedSizes(t *testing.T) {
	sizes := []int{1, 10, 125, 126, 127, 200, 1000, 4096, 8192, 65535, 65536}
	var (
		mu       sync.Mutex
		got      []int
		done     = make(chan struct{})
		wantLens = append([]int{}, sizes...)
	)
	payload := make([]byte, 70000)
	for i := range payload {
		payload[i] = byte(i)
	}

	echo := &funcCallback{onMessage: func(c *Conn, op Opcode, msg []byte) {
		_ = c.WriteMessage(op, msg)
	}}
	recv := &funcCallback{onMessage: func(c *Conn, op Opcode, msg []byte) {
		mu.Lock()
		got = append(got, len(msg))
		if len(got) == len(wantLens) {
			close(done)
		}
		mu.Unlock()
	}}

	url, stop := corkServer(t, echo)
	defer stop()

	con, err := Dial(url, WithClientCallback(recv))
	if err != nil {
		t.Fatal(err)
	}
	defer con.Close()

	// 一批全发出去: 拼成一个 write。
	var buf []byte
	for _, n := range sizes {
		op := Binary
		if n%2 == 0 {
			op = Text
		}
		buf = appendFrame(buf, op, payload[:n])
	}
	writeBatch(t, con, buf)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		mu.Lock()
		t.Fatalf("timeout: got %d/%d lens %v", len(got), len(wantLens), got)
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(wantLens) {
		t.Fatalf("got %d messages, want %d", len(got), len(wantLens))
	}
	for i := range wantLens {
		if got[i] != wantLens[i] {
			t.Errorf("message #%d: got %d bytes, want %d", i, got[i], wantLens[i])
		}
	}
}

// Test_Cork_BatchFragmented 一批里混着分段消息(首片 + 续片)和控制帧。
// 分段的那条要等最后一片到齐才回调, 攒包必须不把它和别的搞混。
//
// 一批里还夹一个 ping: 它排在最前面, 后面的数据帧要紧跟着它按原样解析
// 出来(攒包缓冲区里两边的数据必须各归各位)。这里只断言两个数据帧的
// 内容, 不看 ping——库目前会把控制帧也送进 OnMessage(实测和没有攒包
// 的版本一样, 不是攒包引入的), 那是另一个话题, 这个用例管不着。
func Test_Cork_BatchFragmented(t *testing.T) {
	var (
		mu       sync.Mutex
		gotMsg   []string
		done     = make(chan struct{})
		nonData  = 0
		gotFirst = false
	)
	echo := &funcCallback{onMessage: func(c *Conn, op Opcode, msg []byte) {
		_ = c.WriteMessage(op, msg)
	}}
	recv := &funcCallback{onMessage: func(c *Conn, op Opcode, msg []byte) {
		mu.Lock()
		defer mu.Unlock()
		if op != Binary && op != Text {
			nonData++
			return
		}
		gotMsg = append(gotMsg, string(msg))
		if len(gotMsg) == 1 {
			gotFirst = true
		}
		if len(gotMsg) == 2 && gotFirst {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	}}

	url, stop := corkServer(t, echo)
	defer stop()
	con, err := Dial(url, WithClientCallback(recv))
	if err != nil {
		t.Fatal(err)
	}
	defer con.Close()

	// 一批: ping + 单帧 "A" + 分段 "BBB" + "CCC" (首片 fin=0 + 续片 fin=1)
	var buf []byte
	buf = appendPing(buf, []byte("ping"))
	buf = appendFrame(buf, Binary, []byte("A"))
	buf = appendFrameFin(buf, Text, []byte("BBB"), false) // 首片
	buf = appendFrameFin(buf, Continuation, []byte("CCC"), true)

	writeBatch(t, con, buf)

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		mu.Lock()
		t.Fatalf("timeout: got %v", gotMsg)
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"A", "BBBCCC"}
	if len(gotMsg) != len(want) {
		t.Fatalf("got %v, want %v", gotMsg, want)
	}
	for i := range want {
		if gotMsg[i] != want[i] {
			t.Errorf("#%d: got %q, want %q", i, gotMsg[i], want[i])
		}
	}
	t.Logf("non-data messages (ping/pong): %d", nonData)
}

// Test_Cork_LargeBatchOverflows 一批比攒包缓冲区(16KB)大, 逼它走
// writev 溢出的那条路, 内容仍要对。
//
// 一批 20 × 4096 = 80KB: 远超攒包缓冲区(growCork 换到 16KB 就不再换),
// 所以每一批都会走溢出那条 writev。批次不用更大——这个用例压的是"攒不下
// 的时候不丢不错", 而不是客户端一次能写多少。
func Test_Cork_LargeBatchOverflows(t *testing.T) {
	const (
		perBatch = 20
		payloadN = 4096
		batches  = 10
		total    = perBatch * batches
	)
	var (
		mu       sync.Mutex
		got      = 0
		mismatch int
		done     = make(chan struct{})
	)
	body := make([]byte, payloadN)
	for i := range body {
		body[i] = byte(i * 7)
	}
	echo := &funcCallback{onMessage: func(c *Conn, op Opcode, msg []byte) {
		_ = c.WriteMessage(op, msg)
	}}
	recv := &funcCallback{onMessage: func(c *Conn, op Opcode, msg []byte) {
		mu.Lock()
		got++
		if !bytes.Equal(msg, body) {
			mismatch++
		}
		if got == total {
			close(done)
		}
		mu.Unlock()
	}}

	url, stop := corkServer(t, echo)
	defer stop()
	con, err := Dial(url, WithClientCallback(recv))
	if err != nil {
		t.Fatal(err)
	}
	defer con.Close()

	for b := 0; b < batches; b++ {
		var buf []byte
		for i := 0; i < perBatch; i++ {
			buf = appendFrame(buf, Binary, body)
		}
		writeBatch(t, con, buf)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		mu.Lock()
		t.Fatalf("timeout: got %d/%d", got, total)
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if mismatch > 0 {
		t.Fatalf("%d/%d messages differ", mismatch, total)
	}
}

// appendFrame 追加一个 fin=1 的未掩码 frame。
func appendFrame(b []byte, op Opcode, payload []byte) []byte {
	return appendFrameFin(b, op, payload, true)
}

// appendFrameFin 追加一个指定 fin 的未掩码 frame。
func appendFrameFin(b []byte, op Opcode, payload []byte, fin bool) []byte {
	n := len(payload)
	head := byte(op)
	if fin {
		head |= 0x80
	}
	b = append(b, head)
	switch {
	case n <= 125:
		b = append(b, byte(n))
	case n <= 0xffff:
		b = append(b, 126, byte(n>>8), byte(n))
	default:
		b = append(b, 127)
		var tmp [8]byte
		binary.BigEndian.PutUint64(tmp[:], uint64(n))
		b = append(b, tmp[:]...)
	}
	return append(b, payload...)
}

// appendPing 追加一个 ping 控制帧。
func appendPing(b, payload []byte) []byte {
	if len(payload) > 125 {
		panic(fmt.Sprintf("control frame payload %d > 125", len(payload)))
	}
	b = append(b, 0x80|byte(Ping), byte(len(payload)))
	return append(b, payload...)
}
