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
	"errors"
	"testing"
)

// ---- 变长整数 ----

func TestVarintRoundTrip(t *testing.T) {
	for _, v := range []uint64{
		0, 1, 63, // 1 字节
		64, 16383, // 2 字节
		16384, 1073741823, // 4 字节
		1073741824, 1<<62 - 1, // 8 字节
	} {
		buf := AppendVarint(nil, v)
		got, n, err := ReadVarint(buf)
		if err != nil {
			t.Fatalf("%d: %v", v, err)
		}
		if got != v {
			t.Errorf("%d 往返变成 %d", v, got)
		}
		if n != len(buf) {
			t.Errorf("%d: 读了 %d 字节, 编码是 %d 字节", v, n, len(buf))
		}
		if len(buf) != VarintLen(v) {
			t.Errorf("%d: 编码 %d 字节, VarintLen 说 %d", v, len(buf), VarintLen(v))
		}
	}
}

// 编码长度要符合 RFC 9000 的档位（小的值不能占 8 个字节——变长的意义
// 就在于省字节）。
func TestVarintSizeClasses(t *testing.T) {
	for _, c := range []struct {
		v    uint64
		want int
	}{
		{0, 1}, {63, 1},
		{64, 2}, {16383, 2},
		{16384, 4}, {1073741823, 4},
		{1073741824, 8}, {1<<62 - 1, 8},
	} {
		if got := len(AppendVarint(nil, c.v)); got != c.want {
			t.Errorf("%d 编码成 %d 字节, want %d", c.v, got, c.want)
		}
	}
}

// 掐一半的变长整数要报"不够"，不能读出个错的数。
func TestVarintTruncated(t *testing.T) {
	full := AppendVarint(nil, 16384) // 4 字节
	for i := 1; i < len(full); i++ {
		_, _, err := ReadVarint(full[:i])
		if !errors.Is(err, ErrVarintTooShort) {
			t.Errorf("切 %d 字节: err = %v, want ErrVarintTooShort", i, err)
		}
	}
	// 空的也要报错
	if _, _, err := ReadVarint(nil); !errors.Is(err, ErrVarintTooShort) {
		t.Errorf("空输入: err = %v", err)
	}
}

// 变长整数当长度用：长度 + 内容。
func TestVarintLenPrefixed(t *testing.T) {
	content := []byte("hello")
	buf := AppendVarintLen(nil, content)

	got, n, err := ReadVarintLen(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("内容 = %q, want %q", got, content)
	}
	if n != len(buf) {
		t.Errorf("读了 %d 字节, buf 是 %d 字节", n, len(buf))
	}

	// 内容不全时也要报错
	if _, _, err := ReadVarintLen(buf[:len(buf)-1]); err == nil {
		t.Error("内容不全应该报错")
	}
}

// ---- QUIC 包头 ----

func TestLongHeaderRoundTrip(t *testing.T) {
	destID := []byte{1, 2, 3, 4}
	srcID := []byte{5, 6, 7, 8}

	buf := AppendLongHeader(nil, PacketInitial, 1, destID, srcID, 42, 2)
	buf = append(buf, []byte("payload")...)

	h, err := ReadLongHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if h.Type != PacketInitial {
		t.Errorf("类型 = %d, want Initial", h.Type)
	}
	if h.Version != 1 {
		t.Errorf("版本 = %d, want 1", h.Version)
	}
	if !bytes.Equal(h.DestConnID, destID) {
		t.Errorf("目的连接 ID = %v, want %v", h.DestConnID, destID)
	}
	if !bytes.Equal(h.SrcConnID, srcID) {
		t.Errorf("源连接 ID = %v, want %v", h.SrcConnID, srcID)
	}
	if !bytes.Equal(h.Payload, []byte("payload")) {
		t.Errorf("载荷 = %q", h.Payload)
	}
}

// 长头的四种包类型都要能认出来。
func TestLongHeaderTypes(t *testing.T) {
	for _, ptype := range []byte{PacketInitial, PacketZeroRTT, PacketHandshake, PacketRetry} {
		buf := AppendLongHeader(nil, ptype, 1, []byte{1}, []byte{2}, 0, 1)
		h, err := ReadLongHeader(buf)
		if err != nil {
			t.Fatalf("类型 %d: %v", ptype, err)
		}
		if h.Type != ptype {
			t.Errorf("类型 = %d, want %d", h.Type, ptype)
		}
	}
}

// 版本协商包（版本号 0）：没有连接 ID。
func TestVersionNegotiation(t *testing.T) {
	buf := []byte{0x80 | 0x00<<4, 0, 0, 0, 0, 0, 0, 0, 1}
	h, err := ReadLongHeader(buf)
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != 0 {
		t.Errorf("版本 = %d, want 0", h.Version)
	}
	if len(h.DestConnID) != 0 || len(h.SrcConnID) != 0 {
		t.Errorf("版本协商包不该有连接 ID: %v %v", h.DestConnID, h.SrcConnID)
	}
}

func TestShortHeader(t *testing.T) {
	connID := []byte{9, 8, 7, 6}

	buf := AppendShortHeader(nil, connID, 7, 1)
	buf = append(buf, []byte("data")...)

	if IsLongHeader(buf[0]) {
		t.Fatal("短头的最高位应该是 0")
	}

	h, err := ReadShortHeader(buf, len(connID))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.DestConnID, connID) {
		t.Errorf("连接 ID = %v, want %v", h.DestConnID, connID)
	}
	if h.PacketNumber != 7 {
		t.Errorf("包号 = %d, want 7", h.PacketNumber)
	}
	if !bytes.Equal(h.Payload, []byte("data")) {
		t.Errorf("载荷 = %q", h.Payload)
	}
}

// 连接 ID 超过 20 字节是非法的（RFC 9000: 最长 20）。
func TestConnIDTooLong(t *testing.T) {
	buf := []byte{0x80 | 0x00<<4, 0, 0, 0, 1, 21} // 目的连接 ID 长度 21
	buf = append(buf, make([]byte, 64)...)
	if _, err := ReadLongHeader(buf); err == nil {
		t.Error("21 字节的连接 ID 应该报错")
	}
}

// 包被掐断要报"不够"，不能越界读。
func TestPacketTruncated(t *testing.T) {
	full := AppendLongHeader(nil, PacketInitial, 1, []byte{1, 2, 3}, []byte{4, 5}, 0, 1)
	for i := 1; i < len(full); i++ {
		if _, err := ReadLongHeader(full[:i]); err == nil && i < 5 {
			t.Errorf("切 %d 字节应该报错", i)
		}
	}
}

// ---- HTTP/3 帧 ----

func TestFrameRoundTrip(t *testing.T) {
	var buf []byte
	buf = AppendData(buf, []byte("body"))
	buf = AppendHeaders(buf, []byte("head"))

	p := NewFrameParser()
	var frames []Frame
	n, err := p.Parse(buf, func(f *Frame) error {
		frames = append(frames, Frame{Type: f.Type, Payload: append([]byte(nil), f.Payload...)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Errorf("消化 %d, buf 是 %d", n, len(buf))
	}
	if len(frames) != 2 {
		t.Fatalf("切出 %d 个帧, want 2", len(frames))
	}
	if frames[0].Type != FrameData || string(frames[0].Payload) != "body" {
		t.Errorf("帧 0 = %v %q", frames[0].Type, frames[0].Payload)
	}
	if frames[1].Type != FrameHeaders || string(frames[1].Payload) != "head" {
		t.Errorf("帧 1 = %v %q", frames[1].Type, frames[1].Payload)
	}
}

// 帧被切开喂（QUIC 流是字节流，上层切片随意）。
func TestFrameSplit(t *testing.T) {
	var buf []byte
	buf = AppendData(buf, []byte("hello world, this is a longer body"))

	p := NewFrameParser()
	var frames []Frame
	for i := 0; i < len(buf); i++ {
		if _, err := p.Parse(buf[i:i+1], func(f *Frame) error {
			frames = append(frames, Frame{Type: f.Type, Payload: append([]byte(nil), f.Payload...)})
			return nil
		}); err != nil {
			t.Fatalf("第 %d 字节: %v", i, err)
		}
	}
	if len(frames) != 1 {
		t.Fatalf("切出 %d 个帧", len(frames))
	}
	if string(frames[0].Payload) != "hello world, this is a longer body" {
		t.Errorf("载荷 = %q", frames[0].Payload)
	}
}

// 大长度用变长整数（>2^14 会占 4 个字节，边界要过）。
func TestFrameLargePayload(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 20000) // 长度用 4 字节变长
	var buf []byte
	buf = AppendData(buf, body)

	p := NewFrameParser()
	var got []byte
	if _, err := p.Parse(buf, func(f *Frame) error {
		got = append([]byte(nil), f.Payload...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("载荷 %d 字节, want %d", len(got), len(body))
	}
}

// 长度声明过大要挡住（防对端声明 1GB 让我们一直等）。
func TestFrameTooLong(t *testing.T) {
	var buf []byte
	buf = AppendVarint(buf, uint64(FrameData))
	buf = AppendVarint(buf, 100*1024*1024) // 100MB
	buf = append(buf, make([]byte, 100)...)

	p := NewFrameParser()
	_, err := p.Parse(buf, func(*Frame) error { return nil })
	if !errors.Is(err, ErrFrameTooLong) {
		t.Fatalf("err = %v, want ErrFrameTooLong", err)
	}
}

// 一次喂半个帧，不该报错也不该产出帧。
//
// 每次**换一个全新的解析器**（不是拿同一段前缀反复喂）：解析器会把不够
// 的部分攒着，反复喂同一段会越攒越长（那是重复的字节，不是"数据在增长"）。
// 真实的用法是"喂一次、攒着、下次喂新读到的"。
func TestFramePartial(t *testing.T) {
	var buf []byte
	buf = AppendData(buf, []byte("abcdef"))

	for _, cut := range []int{1, 2, len(buf) - 1} {
		p := NewFrameParser()
		n, err := p.Parse(buf[:cut], func(*Frame) error {
			t.Fatal("不该有帧")
			return nil
		})
		if err != nil {
			t.Fatalf("切 %d 字节报错: %v", cut, err)
		}
		if n != cut {
			t.Errorf("切 %d 字节: 消化 %d(应该全收下)", cut, n)
		}
		if p.Buffered() != cut {
			t.Errorf("切 %d 字节: 攒了 %d, 应该全攒着", cut, p.Buffered())
		}
	}
}

// SETTINGS 往返。
func TestSettingsRoundTrip(t *testing.T) {
	settings := []Setting{
		{ID: SettingQPACKMaxTableCapacity, Value: 4096},
		{ID: SettingMaxFieldSectionSize, Value: 65536},
		{ID: SettingH3Datagram, Value: 1},
	}
	buf := AppendSettings(nil, settings...)

	p := NewFrameParser()
	var frame *Frame
	if _, err := p.Parse(buf, func(f *Frame) error {
		frame = f
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if frame == nil || frame.Type != FrameSettings {
		t.Fatalf("没解出 SETTINGS: %+v", frame)
	}

	got, err := ParseSettings(frame.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(settings) {
		t.Fatalf("解出 %d 项, want %d", len(got), len(settings))
	}
	for i := range settings {
		if got[i] != settings[i] {
			t.Errorf("#%d = %+v, want %+v", i, got[i], settings[i])
		}
	}
}

// 流 ID 的判断：请求流是客户端发起的双向流（低两位 00）。
func TestRequestStream(t *testing.T) {
	for _, c := range []struct {
		id   uint64
		want bool
	}{
		{0, true}, {4, true}, {8, true}, // 客户端双向
		{1, false}, {2, false}, {3, false}, // 其他三类
		{5, false}, {6, false}, {7, false},
	} {
		if got := IsRequestStream(c.id); got != c.want {
			t.Errorf("IsRequestStream(%d) = %v, want %v", c.id, got, c.want)
		}
	}
}

// 帧类型名（日志里好认）。
func TestFrameTypeString(t *testing.T) {
	for _, c := range []struct {
		t    FrameType
		want string
	}{
		{FrameData, "DATA"},
		{FrameHeaders, "HEADERS"},
		{FrameSettings, "SETTINGS"},
		{FrameGoAway, "GOAWAY"},
		{FrameType(0xff), "UNKNOWN(0xff)"},
	} {
		if got := c.t.String(); got != c.want {
			t.Errorf("%d.String() = %q, want %q", c.t, got, c.want)
		}
	}
}
