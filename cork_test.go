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
package greatws

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Test_Cork_EchoBatch 一次 write 里塞多条消息, 校验服务端把这一批全部
// 回显, 内容不串、顺序不乱, 而且确实走了攒包(回包的写系统调用数比消息
// 数少)。
//
// 客户端每条消息单独攒起来再一次写: 服务端才会在一次 read 里拿到多个
// frame, 这正是攒包的触发条件(一次 WriteMessage 是一次 syscall, 单独
// 发的话服务端一次 read 可能只有一个 frame)。
func Test_Cork_EchoBatch(t *testing.T) {
	const (
		batches    = 200  // 发多少批
		perBatch   = 10   // 每批多少条
		payloadN   = 1024 // 每条多大
		msgCount   = batches * perBatch
		batchBytes = perBatch * (payloadN + 10)
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

	// 服务端: 原样回显。
	echoCb := &funcCallback{
		onMessage: func(c *Conn, op Opcode, msg []byte) {
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

	before := m.GetWriteSyscallNum()

	// 一次 write 里塞一批: 把每条消息的 frame 拼起来, 直接写连接。
	// 服务端那侧的写系统调用数记在 before/after 之间, 客户端自己发的
	// 也算进去(和回包一比一), 所以断言用"每条消息的写系统调用数":
	// 攒包生效时服务端每批只写一次, 而不是每条一次。
	var buf []byte
	for i := 0; i < msgCount; i++ {
		if i%perBatch == 0 {
			buf = buf[:0]
		}
		buf = appendFrame(buf, Binary, want(i))
		if i%perBatch == perBatch-1 {
			func() {
				con.mu.Lock()
				defer con.mu.Unlock()
				if _, err := con.write(buf); err != nil {
					t.Fatalf("write batch %d: %v", i/perBatch, err)
				}
			}()
			// 给服务端一点时间处理这一批, 别把几十批都堆在一次 read 里。
			if i/perBatch%20 == 19 {
				time.Sleep(time.Millisecond)
			}
		}
	}

	select {
	case <-done:
	case <-time.After(30 * time.Second):
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

	// 客户端发了 batches 次(每次一批), 服务端回 batches 次, 两边各写
	// 一次; 攒包不生效时服务端是每条一次, 一共 msgCount + batches 次。
	// 断言一个宽松但明确的上界: 明显少于"每条一次"。
	writes := m.GetWriteSyscallNum() - before
	if writes > int64(msgCount/2) {
		t.Fatalf("write syscalls = %d for %d messages (%d batches): 攒包没生效",
			writes, msgCount, batches)
	}
	t.Logf("write syscalls: %d for %d messages in %d batches (batchBytes=%d)",
		writes, msgCount, batches, batchBytes)
}
