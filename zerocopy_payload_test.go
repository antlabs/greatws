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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Test_ZeroCopyPayload_Echo 连续发一批带序号的消息, 校验每一条回来的
// 内容都对得上。
//
// 这个测试专门盯零拷贝那条路的坑: payload 是读缓冲区的一段别名, 如果
// 哪一步把序号之间的数据错开了(比如 rr 没推进、或者 leftMove 把回调正在
// 用的那段搬走了), 内容就会串。所以每条消息都自成一体、带自己的指纹,
// 对不上立刻能看出来。
func Test_ZeroCopyPayload_Echo(t *testing.T) {
	const (
		msgCount = 2000
		payloadN = 300
	)

	// want 按序号造一条消息: 前 8 字节是序号, 其余按序号填充。
	want := func(i int) []byte {
		b := make([]byte, payloadN)
		copy(b, fmt.Sprintf("%08d", i))
		for j := 8; j < len(b); j++ {
			b[j] = byte(i + j)
		}
		return b
	}

	var (
		mu       sync.Mutex
		got      = 0
		mismatch []string
		done     = make(chan struct{})
	)

	// echoCb 把收到的原样写回去。零拷贝时 msg 就是读缓冲区的一段, 而
	// WriteMessage 会立刻把它写进 socket, 所以这里正好压到"回调期间用完"
	// 这条契约上。
	//
	// 服务端这一侧才是被测对象(它开了 WithServerZeroCopyPayload), 所以
	// 别名计数记在这里: 零拷贝那份是 rbuf[rr:rr+N:rr+N], 全切片表达式
	// 截出来的 cap 正好等于 len; 走内存池拷贝的那条 cap 必然大于 len
	// (池子给的是 len+MaxFrameHeaderSize 起步)。少了这个计数, 测试可能在
	// "优化根本没生效"的情况下照样全绿。
	aliased := int32(0)
	echoCb := &funcCallback{
		onMessage: func(c *Conn, op Opcode, msg []byte) {
			if cap(msg) == len(msg) && len(msg) > 0 {
				atomic.AddInt32(&aliased, 1)
			}
			if err := c.WriteMessage(op, msg); err != nil {
				t.Error(err)
			}
		},
	}

	gotCb := &funcCallback{
		onMessage: func(c *Conn, op Opcode, msg []byte) {
			mu.Lock()
			i := got
			got++
			exp := want(i)
			if !bytes.Equal(msg, exp) {
				if len(mismatch) < 5 {
					mismatch = append(mismatch, fmt.Sprintf(
						"#%d: got %d bytes %q..., want %q...", i, len(msg), head(msg), head(exp)))
				}
			}
			if got == msgCount {
				close(done)
			}
			mu.Unlock()
		},
	}

	m := NewMultiEventLoopAndStartMust(
		WithEventLoops(1),
		WithLogLevel(slog.LevelError),
		WithBusinessGoNum(1, 1, 1),
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r,
			WithServerCallback(echoCb),
			WithServerMultiEventLoop(m),
			WithServerZeroCopyPayload(),
		)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		<-done
	}))
	defer ts.Close()

	con, err := Dial(strings.ReplaceAll(ts.URL, "http", "ws"),
		WithClientCallback(gotCb), WithClientMultiEventLoop(m))
	if err != nil {
		t.Fatal(err)
	}
	defer con.Close()

	for i := 0; i < msgCount; i++ {
		if err := con.WriteMessage(Binary, want(i)); err != nil {
			t.Fatalf("write #%d: %v", i, err)
		}
	}

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		mu.Lock()
		t.Fatalf("timeout: got %d/%d", got, msgCount)
		mu.Unlock()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(mismatch) > 0 {
		t.Fatalf("payload mismatch:\n%s", strings.Join(mismatch, "\n"))
	}
	if got != msgCount {
		t.Fatalf("got %d, want %d", got, msgCount)
	}
	// 服务端收到了 msgCount 条、每条都回显了, 所以别名计数应该正好是
	// msgCount; 是 0 就说明快路径压根没走到, 这个测试没验到东西。
	if n := int(atomic.LoadInt32(&aliased)); n != msgCount {
		t.Fatalf("zero-copy path taken %d/%d 次: 期望每条都是读缓冲区的别名", n, msgCount)
	}
	t.Logf("aliased payloads: %d/%d", atomic.LoadInt32(&aliased), msgCount)
}

func head(b []byte) string {
	if len(b) > 16 {
		return string(b[:16])
	}
	return string(b)
}

// funcCallback 是一个把每个回调转发到函数上的 Callback。
type funcCallback struct {
	onOpen    func(*Conn)
	onMessage func(*Conn, Opcode, []byte)
	onClose   func(*Conn, error)
}

func (f *funcCallback) OnOpen(c *Conn) {
	if f.onOpen != nil {
		f.onOpen(c)
	}
}

func (f *funcCallback) OnMessage(c *Conn, op Opcode, msg []byte) {
	if f.onMessage != nil {
		f.onMessage(c, op, msg)
	}
}

func (f *funcCallback) OnClose(c *Conn, err error) {
	if f.onClose != nil {
		f.onClose(c, err)
	}
}
