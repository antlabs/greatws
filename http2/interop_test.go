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

package http2

import (
	"bytes"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// 跨实现验证：用 **Go 官方的 x/net/http2.Framer** 编帧喂给我们，
// 再用它解我们发出去的帧。
//
// 为什么这一步重要：自己写的帧编解码 + 自己写的解析器，两边用同一套
// 假设，错了也互相"对得上"。官方的 Framer 是另一套独立实现（而且被
// 全世界的 HTTP/2 客户端/服务端用着），它认了才说明我们的帧是合法的。
//
// 这一层是 HTTP/2 最容易出错的地方：9 字节头的位域、CONTINUATION 的
// 顺序、SETTINGS 的约束——都是"写错了自己测不出来，但真实客户端一接
// 就崩"的东西。

// officialFrame 是官方 Framer 编出来的一帧（原始字节）。
func officialFrame(t *testing.T, write func(*http2.Framer) error) []byte {
	t.Helper()
	var buf bytes.Buffer
	fr := http2.NewFramer(&buf, nil)
	if err := write(fr); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// hpackBlock 用官方的 HPACK 编码器编一个头块。
func hpackBlock(t *testing.T, fields ...hpack.HeaderField) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	for _, f := range fields {
		if err := enc.WriteField(f); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// 官方编的 HEADERS，我们解出来要对。
func TestOfficialFramerHeaders(t *testing.T) {
	block := hpackBlock(t,
		hpack.HeaderField{Name: ":method", Value: "POST"},
		hpack.HeaderField{Name: ":path", Value: "/greeter.SayHello"},
		hpack.HeaderField{Name: ":scheme", Value: "https"},
		hpack.HeaderField{Name: "content-type", Value: "application/grpc"},
	)

	raw := officialFrame(t, func(fr *http2.Framer) error {
		return fr.WriteHeaders(http2.HeadersFrameParam{
			StreamID:      1,
			BlockFragment: block,
			EndHeaders:    true,
			EndStream:     false,
		})
	})

	rec := &recorder{}
	conn := NewConn(false, rec)

	// 服务端要先看到连接序言
	if _, _, err := conn.Feed(append(append([]byte(nil), clientPreface...), raw...)); err != nil {
		t.Fatalf("Feed: %v", err)
	}

	if len(rec.headers) != 1 {
		t.Fatalf("收到 %d 个 HEADERS, want 1", len(rec.headers))
	}
	got := map[string]string{}
	for _, f := range rec.headers[0].fields {
		got[f.Name] = f.Value
	}
	want := map[string]string{
		":method":      "POST",
		":path":        "/greeter.SayHello",
		":scheme":      "https",
		"content-type": "application/grpc",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// 官方编的 DATA，我们要原样收到。
func TestOfficialFramerData(t *testing.T) {
	rec := &recorder{}
	conn := NewConn(false, rec)
	if _, _, err := conn.Feed(clientPreface); err != nil {
		t.Fatal(err)
	}

	// **先建流再发 DATA**：DATA 只能发在已经开了的流上（RFC 9113 5.1，
	// idle 上收 DATA 是连接级错误）。这条以前是直接往 stream 3 发 DATA
	// ——在没做状态检查的时候能过，加了检查之后就是"正确地把协议错误
	// 报出来"了。
	raw := officialFrame(t, func(fr *http2.Framer) error {
		block := hpackBlock(t,
			hpack.HeaderField{Name: ":method", Value: "POST"},
			hpack.HeaderField{Name: ":scheme", Value: "http"},
			hpack.HeaderField{Name: ":path", Value: "/data"},
		)
		if err := fr.WriteHeaders(http2.HeadersFrameParam{
			StreamID: 3, BlockFragment: block, EndHeaders: true,
		}); err != nil {
			return err
		}
		return fr.WriteData(3, true, []byte("hello from the official framer"))
	})

	if _, _, err := conn.Feed(raw); err != nil {
		t.Fatal(err)
	}
	if string(rec.data) != "hello from the official framer" {
		t.Errorf("收到 %q", rec.data)
	}
}

// 官方编的 SETTINGS / PING / RST_STREAM / GOAWAY，我们都要正确应答。
func TestOfficialFramerControlFrames(t *testing.T) {
	t.Run("SETTINGS", func(t *testing.T) {
		raw := officialFrame(t, func(fr *http2.Framer) error {
			return fr.WriteSettings(http2.Setting{ID: http2.SettingMaxFrameSize, Val: 32768})
		})
		conn := NewConn(false, &recorder{})
		_, out, err := conn.Feed(append(append([]byte(nil), clientPreface...), raw...))
		if err != nil {
			t.Fatal(err)
		}
		// 我们的回应必须能被官方 Framer 解出来
		assertIsSettingsAck(t, out)
	})

	t.Run("PING", func(t *testing.T) {
		payload := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
		raw := officialFrame(t, func(fr *http2.Framer) error {
			return fr.WritePing(false, payload)
		})
		conn := NewConn(false, &recorder{})
		_, out, err := conn.Feed(append(append([]byte(nil), clientPreface...), raw...))
		if err != nil {
			t.Fatal(err)
		}
		fr := http2.NewFramer(nil, bytes.NewReader(out))
		f, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("官方 Framer 解我们的回应: %v", err)
		}
		pf, ok := f.(*http2.PingFrame)
		if !ok {
			t.Fatalf("回应是 %T, want *http2.PingFrame", f)
		}
		if !pf.IsAck() {
			t.Error("回的不是 PING ACK")
		}
		if pf.Data != payload {
			t.Errorf("ACK 载荷 = %v, want %v", pf.Data, payload)
		}
	})

	t.Run("RST_STREAM", func(t *testing.T) {
		raw := officialFrame(t, func(fr *http2.Framer) error {
			return fr.WriteRSTStream(5, http2.ErrCodeCancel)
		})
		rec := &recorder{}
		conn := NewConn(false, rec)
		// 先建个流
		_, _, _ = conn.Feed(clientPreface)
		conn.stream(5)
		if _, _, err := conn.Feed(raw); err != nil {
			t.Fatal(err)
		}
		if rec.rst != 5 || rec.rstCode != ErrCodeCancel {
			t.Errorf("RST 通知 = %d/%v", rec.rst, rec.rstCode)
		}
	})

	t.Run("GOAWAY", func(t *testing.T) {
		raw := officialFrame(t, func(fr *http2.Framer) error {
			return fr.WriteGoAway(9, http2.ErrCodeEnhanceYourCalm, []byte("slow down"))
		})
		conn := NewConn(false, &recorder{})
		_, _, _ = conn.Feed(clientPreface)
		if _, _, err := conn.Feed(raw); err != nil {
			t.Fatal(err)
		}
		if !conn.GoAway() {
			t.Error("没记下 GOAWAY")
		}
		if conn.GoAwayCode() != ErrCodeEnhanceCalm {
			t.Errorf("GOAWAY 码 = %v", conn.GoAwayCode())
		}
	})
}

func assertIsSettingsAck(t *testing.T, out []byte) {
	t.Helper()
	fr := http2.NewFramer(nil, bytes.NewReader(out))
	f, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("官方 Framer 解我们的回应: %v", err)
	}
	sf, ok := f.(*http2.SettingsFrame)
	if !ok {
		t.Fatalf("回应是 %T, want *http2.SettingsFrame", f)
	}
	if !sf.IsAck() {
		t.Error("回的不是 SETTINGS ACK")
	}
}

// **我们发出去的帧，官方 Framer 要能解。**
//
// 这个方向比"能解官方的帧"更重要：真实客户端（浏览器、grpc-go）都用
// 自己的实现解我们的帧，解不了就是连接直接断。
func TestOurFramesDecodableByOfficial(t *testing.T) {
	server := NewConn(false, &recorder{})
	if _, _, err := server.Feed(clientPreface); err != nil {
		t.Fatal(err)
	}

	// 我们发响应头和响应体
	if err := server.WriteHeaders(1, []HeaderField{
		{Name: ":status", Value: "200"},
		{Name: "content-type", Value: "application/grpc"},
	}, false); err != nil {
		t.Fatal(err)
	}
	if err := server.WriteData(1, []byte("response body"), false); err != nil {
		t.Fatal(err)
	}
	if err := server.WriteTrailers(1, []HeaderField{
		{Name: "grpc-status", Value: "0"},
	}); err != nil {
		t.Fatal(err)
	}

	out := server.TakeOutput()
	if len(out) == 0 {
		t.Fatal("没有输出")
	}

	// 用官方 Framer 解
	fr := http2.NewFramer(nil, bytes.NewReader(out))
	var sawHeaders, sawData, sawTrailers bool
	dec := hpack.NewDecoder(4096, nil)

	for {
		f, err := fr.ReadFrame()
		if err != nil {
			break
		}
		switch v := f.(type) {
		case *http2.HeadersFrame:
			fields, derr := dec.DecodeFull(v.HeaderBlockFragment())
			if derr != nil {
				t.Fatalf("官方 HPACK 解我们的头块: %v", derr)
			}
			m := map[string]string{}
			for _, x := range fields {
				m[x.Name] = x.Value
			}
			if v.StreamID == 1 && m[":status"] == "200" {
				sawHeaders = true
			}
			if m["grpc-status"] == "0" {
				sawTrailers = true
				if !v.StreamEnded() {
					t.Error("trailer 应该带 END_STREAM")
				}
			}
		case *http2.DataFrame:
			if string(v.Data()) == "response body" {
				sawData = true
			}
		}
	}

	if !sawHeaders {
		t.Error("官方 Framer 没解出我们的响应头")
	}
	if !sawData {
		t.Error("官方 Framer 没解出我们的响应体")
	}
	if !sawTrailers {
		t.Error("官方 Framer 没解出我们的 trailer")
	}
}

// 大响应体（跨多个 DATA 帧）官方 Framer 也要能拼回来。
func TestOurLargeDataDecodableByOfficial(t *testing.T) {
	server := NewConn(false, &recorder{})
	_, _, _ = server.Feed(clientPreface)

	// 让分片小一点，逼它拆帧
	server.peerMaxFrameSize = 1024

	body := bytes.Repeat([]byte("abcdefgh"), 1024) // 8KB
	if err := server.WriteData(1, body, true); err != nil {
		t.Fatal(err)
	}
	out := server.TakeOutput()

	fr := http2.NewFramer(nil, bytes.NewReader(out))
	var got []byte
	frames := 0
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			break
		}
		if d, ok := f.(*http2.DataFrame); ok {
			frames++
			got = append(got, d.Data()...)
		}
	}
	if frames < 2 {
		t.Fatalf("切出 %d 个 DATA 帧，应该拆成多个（8KB / 1KB）", frames)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("拼回来 %d 字节, want %d", len(got), len(body))
	}
}

// 官方 SETTINGS 里带各种设置，我们都要正确处理并不报错。
func TestOfficialSettingsVariants(t *testing.T) {
	settings := []http2.Setting{
		{ID: http2.SettingHeaderTableSize, Val: 8192},
		{ID: http2.SettingEnablePush, Val: 0},
		{ID: http2.SettingMaxConcurrentStreams, Val: 100},
		{ID: http2.SettingInitialWindowSize, Val: 1 << 20},
		{ID: http2.SettingMaxFrameSize, Val: 1 << 15},
		{ID: http2.SettingMaxHeaderListSize, Val: 1 << 16},
	}
	raw := officialFrame(t, func(fr *http2.Framer) error {
		return fr.WriteSettings(settings...)
	})

	conn := NewConn(false, &recorder{})
	_, out, err := conn.Feed(append(append([]byte(nil), clientPreface...), raw...))
	if err != nil {
		t.Fatalf("处理官方 SETTINGS 报错: %v", err)
	}
	assertIsSettingsAck(t, out)

	if conn.PeerMaxFrameSize() != 1<<15 {
		t.Errorf("对端最大帧大小 = %d, want %d", conn.PeerMaxFrameSize(), 1<<15)
	}
}
