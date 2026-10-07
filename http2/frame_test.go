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
	"errors"
	"testing"
)

// parseAll 把 buf 全喂进去，返回所有帧（载荷拷贝出来，因为原来的切片
// 只在回调期间有效）。
func parseAll(t *testing.T, p *FrameParser, buf []byte) []Frame {
	t.Helper()
	var frames []Frame
	n, err := p.Parse(buf, func(f *Frame) error {
		payload := append([]byte(nil), f.Payload...)
		frames = append(frames, Frame{
			Type:     f.Type,
			Flags:    f.Flags,
			StreamID: f.StreamID,
			Payload:  payload,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("消化了 %d 字节, 喂进去 %d", n, len(buf))
	}
	return frames
}

func TestFrameRoundTrip(t *testing.T) {
	// 拼几个帧，切回来要对得上
	var buf []byte
	buf = AppendSettings(buf, [2]uint32{0x3, 100})
	buf = AppendPing(buf, [8]byte{1, 2, 3, 4, 5, 6, 7, 8}, false)
	buf = AppendHeaders(buf, 1, []byte("headerblock"), true, true)
	buf = AppendData(buf, 1, []byte("hello"), true)
	buf = AppendRSTStream(buf, 3, uint32(ErrCodeCancel))
	buf = AppendWindowUpdate(buf, 1, 1024)
	buf = AppendGoAway(buf, 5, uint32(ErrCodeNo), []byte("bye"))

	frames := parseAll(t, NewFrameParser(), buf)
	if len(frames) != 7 {
		t.Fatalf("切出 %d 个帧, want 7", len(frames))
	}

	if frames[0].Type != FrameSettings || frames[0].StreamID != 0 {
		t.Errorf("帧 0: %v stream=%d", frames[0].Type, frames[0].StreamID)
	}
	if len(frames[0].Payload) != 6 {
		t.Errorf("SETTINGS 载荷 %d 字节, want 6", len(frames[0].Payload))
	}
	if frames[1].Type != FramePing || !bytes.Equal(frames[1].Payload, []byte{1, 2, 3, 4, 5, 6, 7, 8}) {
		t.Errorf("PING: %v %v", frames[1].Type, frames[1].Payload)
	}
	if frames[2].Type != FrameHeaders || !frames[2].EndStream() || !frames[2].EndHeaders() {
		t.Errorf("HEADERS 标志不对: flags=%#x", frames[2].Flags)
	}
	if frames[3].Type != FrameData || string(frames[3].Payload) != "hello" || !frames[3].EndStream() {
		t.Errorf("DATA: %v %q", frames[3].Type, frames[3].Payload)
	}
	if frames[4].Type != FrameRSTStream || frames[4].StreamID != 3 {
		t.Errorf("RST_STREAM: %v stream=%d", frames[4].Type, frames[4].StreamID)
	}
	if frames[5].Type != FrameWindowUpdate {
		t.Errorf("WINDOW_UPDATE: %v", frames[5].Type)
	}
	if frames[6].Type != FrameGoAway || !bytes.Contains(frames[6].Payload, []byte("bye")) {
		t.Errorf("GOAWAY: %v %q", frames[6].Type, frames[6].Payload)
	}
}

// 帧被切开喂（TCP 会在任意位置切），要能拼回来。
//
// **契约**：没切出整帧的字节要留着，下次和新读到的拼一起再喂（引擎的
// readAndDispatch 就是这么做的）。解析器自己不缓冲——缓冲区在调用方那里，
// 那是它本来就有的（读缓冲区）。
func TestFrameSplitAcrossReads(t *testing.T) {
	var buf []byte
	buf = AppendData(buf, 1, []byte("hello world"), true)

	p := NewFrameParser()
	var pending []byte
	var got []Frame

	for i := 0; i < len(buf); i++ {
		pending = append(pending, buf[i])
		n, err := p.Parse(pending, func(f *Frame) error {
			got = append(got, Frame{Type: f.Type, StreamID: f.StreamID,
				Payload: append([]byte(nil), f.Payload...)})
			return nil
		})
		if err != nil {
			t.Fatalf("第 %d 字节: %v", i, err)
		}
		pending = pending[n:]
		if len(got) > 0 {
			break
		}
	}
	if len(got) != 1 {
		t.Fatalf("逐字节喂之后切出 %d 个帧, want 1", len(got))
	}
	if string(got[0].Payload) != "hello world" {
		t.Errorf("载荷 = %q", got[0].Payload)
	}
	if len(pending) != 0 {
		t.Errorf("切出帧之后还剩 %d 字节没消化", len(pending))
	}
}

// 帧头里的长度声明错了要报错（不能读到缓冲区外面去）。
func TestBadFrameShape(t *testing.T) {
	// 拼一个"帧头声明 n 字节载荷"的帧，载荷内容随便填。
	// 载荷必须真的拼上：只拼 9 字节头的话，那是"帧没收全"，解析器会
	// 静静地等下一次（不报错）——这两件事要分清楚。
	mk := func(ftype FrameType, flags uint8, streamID uint32, n int) []byte {
		b := AppendFrameHeader(nil, ftype, flags, streamID, n)
		return append(b, make([]byte, n)...)
	}

	for _, c := range []struct {
		name string
		buf  []byte
	}{
		{"DATA 在流 0 上", mk(FrameData, 0, 0, 1)},
		{"RST_STREAM 载荷不是 4 字节", mk(FrameRSTStream, 0, 1, 3)},
		{"PING 载荷不是 8 字节", mk(FramePing, 0, 0, 4)},
		{"SETTINGS 在流上(不是 0)", mk(FrameSettings, 0, 1, 0)},
		{"GOAWAY 载荷不足 8 字节", mk(FrameGoAway, 0, 0, 4)},
		{"WINDOW_UPDATE 载荷不是 4 字节", mk(FrameWindowUpdate, 0, 1, 2)},
		{"CONTINUATION 在流 0 上", mk(FrameContinuation, FlagContinuationEndHeaders, 0, 0)},
		{"PRIORITY 载荷不是 5 字节", mk(FramePriority, 0, 1, 3)},
	} {
		p := NewFrameParser()
		_, err := p.Parse(c.buf, func(*Frame) error { return nil })
		if err == nil {
			t.Errorf("%s: 应该报错", c.name)
		}
	}
}

// 帧只到一半（声明 10 字节只给了 3 字节）不该报错，要静静地等下一次——
// 这是非阻塞 io 的核心契约，和"帧形状不对"是两回事。
func TestPartialFrameIsNotAnError(t *testing.T) {
	full := AppendData(nil, 1, []byte("0123456789"), false)

	p := NewFrameParser()
	for _, cut := range []int{0, 1, 8, 9, 15, len(full) - 1} {
		n, err := p.Parse(full[:cut], func(*Frame) error {
			t.Fatal("不该有帧切出来")
			return nil
		})
		if err != nil {
			t.Fatalf("切 %d 字节时报错了: %v", cut, err)
		}
		// 返回值是"变成了几个完整帧的字节数"，不是"看了多少字节"。
		// 一帧都没切出来就是 0，调用方把整个 pending 留着。
		if n != 0 {
			t.Errorf("切 %d 字节: 消化量 %d, 应该一个帧都没切出来(0)", cut, n)
		}
	}

	// 全给了就该切出来
	var got []Frame
	if _, err := p.Parse(full, func(f *Frame) error {
		got = append(got, Frame{Type: f.Type, Payload: append([]byte(nil), f.Payload...)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0].Payload) != "0123456789" {
		t.Fatalf("切出 %d 个帧: %v", len(got), got)
	}
}

// SETTINGS ACK 不能带载荷（RFC 9113 6.5）。
func TestSettingsAckNoPayload(t *testing.T) {
	buf := AppendFrameHeader(nil, FrameSettings, FlagSettingsAck, 0, 6)
	buf = append(buf, 0, 0, 0, 0, 0, 0)
	p := NewFrameParser()
	if _, err := p.Parse(buf, func(*Frame) error { return nil }); err == nil {
		t.Fatal("带载荷的 SETTINGS ACK 应该报错")
	}
}

// SETTINGS 载荷必须是 6 的倍数。
func TestSettingsPayloadMultipleOf6(t *testing.T) {
	buf := AppendFrameHeader(nil, FrameSettings, 0, 0, 5)
	buf = append(buf, 0, 0, 0, 0, 0)
	p := NewFrameParser()
	if _, err := p.Parse(buf, func(*Frame) error { return nil }); err == nil {
		t.Fatal("载荷不是 6 的倍数的 SETTINGS 应该报错")
	}
}

// 帧超过 SETTINGS 声明的上限要报错（防对端用一个超大帧把内存吃光）。
func TestFrameTooLarge(t *testing.T) {
	var buf []byte
	buf = AppendData(buf, 1, make([]byte, 20000), false)

	p := NewFrameParser()
	// 默认 16384，20000 超了
	if _, err := p.Parse(buf, func(*Frame) error { return nil }); err == nil {
		t.Fatal("超过上限的帧应该报错")
	}

	// 调大之后应该能过
	p.SetMaxFrameSize(32768)
	if _, err := p.Parse(buf, func(*Frame) error { return nil }); err != nil {
		t.Fatalf("调大上限之后还是报错: %v", err)
	}
}

// CONTINUATION 必须紧跟前一个头帧（RFC 9113 6.10）——不然后面两个流的
// 头块会穿插，HPACK 是有状态的，没法解。
func TestContinuationOrdering(t *testing.T) {
	// 没有前置 HEADERS 的 CONTINUATION
	p := NewFrameParser()
	buf := AppendFrameHeader(nil, FrameContinuation, FlagContinuationEndHeaders, 1, 0)
	if _, err := p.Parse(buf, func(*Frame) error { return nil }); err == nil {
		t.Fatal("没有前置 HEADERS 的 CONTINUATION 应该报错")
	}

	// HEADERS(流1, 没 END_HEADERS) 之后跟一个流 3 的 CONTINUATION
	p = NewFrameParser()
	buf = AppendFrameHeader(nil, FrameHeaders, 0, 1, 1)
	buf = append(buf, 'x')
	buf = AppendFrameHeader(nil, FrameContinuation, FlagContinuationEndHeaders, 3, 0)
	if _, err := p.Parse(buf, func(*Frame) error { return nil }); err == nil {
		t.Fatal("换了流的 CONTINUATION 应该报错")
	}

	// 正确顺序：HEADERS + CONTINUATION（同一个流）
	//
	// 注意这里用同一个 buf 接着拼（不是 AppendFrameHeader(nil, ...)——
	// 那样会把前面的帧丢掉，测试就测了个寂寞）。
	p = NewFrameParser()
	buf = AppendFrameHeader(nil, FrameHeaders, FlagHeadersEndStream, 1, 1)
	buf = append(buf, 'x')
	buf = AppendFrameHeader(buf, FrameContinuation, FlagContinuationEndHeaders, 1, 1)
	buf = append(buf, 'y')
	frames := parseAll(t, p, buf)
	if len(frames) != 2 {
		t.Fatalf("切出 %d 个帧, want 2", len(frames))
	}
}

// 回调返回错误要停下来，消化量停在出错的那个帧之前。
func TestCallbackErrorStops(t *testing.T) {
	var buf []byte
	buf = AppendData(buf, 1, []byte("aaa"), false)
	buf = AppendData(buf, 1, []byte("bbb"), false)

	p := NewFrameParser()
	boom := errors.New("boom")
	count := 0
	n, err := p.Parse(buf, func(*Frame) error {
		count++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if count != 1 {
		t.Errorf("回调被调了 %d 次, want 1", count)
	}
	if n != 0 {
		t.Errorf("出错时消化量应该是 0(第一个帧就没处理完), got %d", n)
	}
}

// 流 ID 的最高位是保留位，要屏蔽掉（RFC 9113 4.1）。
func TestStreamIDReservedBit(t *testing.T) {
	// 手工拼一个流 ID 最高位为 1 的 DATA 头
	buf := []byte{
		0, 0, 3, // 长度 3
		byte(FrameData), // 类型
		0,               // 标志
		0x80, 0, 0, 1,   // 流 ID 1 但最高位是 1
		'a', 'b', 'c',
	}
	frames := parseAll(t, NewFrameParser(), buf)
	if len(frames) != 1 {
		t.Fatalf("切出 %d 个帧", len(frames))
	}
	if frames[0].StreamID != 1 {
		t.Errorf("流 ID = %d, want 1(保留位要屏蔽)", frames[0].StreamID)
	}
}
