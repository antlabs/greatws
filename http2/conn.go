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
	"errors"
	"fmt"
	"sync"
)

// 连接级的状态。
type connState uint8

const (
	// statePreface 还没收到客户端的连接序言
	statePreface connState = iota
	// stateSettings 序言收到了，等 SETTINGS
	stateSettings
	// stateOpen 正常收发
	stateOpen
	// stateClosed 收到/发出 GOAWAY，连接要关
	stateClosed
)

// clientPreface 是 HTTP/2 的固定连接序言（RFC 9113 3.4）。
//
// 客户端连上之后第一件事就是发这 24 字节，服务端看到了才认为这是 HTTP/2
// 连接。这段字符串是规范写死的。
var clientPreface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// StreamHandler 是流上面的事件回调。
//
// 和 engine.Handler 是一个路子：协议层（比如 gRPC）实现它，http2 把流上
// 的请求头、数据、结束通知交给它。
type StreamHandler interface {
	// OnHeaders 一个流的请求头解完了。pseudo 是 :method :path 这些。
	OnHeaders(c *Conn, streamID uint32, headers []HeaderField, endStream bool)

	// OnData 流上的数据。末尾那次 endStream 为真。
	OnData(c *Conn, streamID uint32, data []byte, endStream bool)

	// OnRSTStream 对端重置了一个流。
	OnRSTStream(c *Conn, streamID uint32, code ErrCode)
}

// Conn 是一条 HTTP/2 连接的状态机。
//
// 它跑在 engine 上：引擎喂字节（OnData），这里切帧、解 HPACK、管流，
// 需要发的帧通过 Write 交给引擎。
//
// HPACK 的编解码器是**连接级**的（动态表跨头块保留），所以
// 一个 HTTP/2 连接一个 Conn，不能共用。
type Conn struct {
	// 发送那边
	mu      sync.Mutex
	outBuf  []byte
	encoder *hpackEncoder

	// 接收那边
	parser *FrameParser
	dec    *hpackDecoder

	state connState

	// 客户端的流 ID 是奇数，服务端发起的（push）是偶数
	nextStreamID uint32
	// isClient 我们这边是客户端还是服务端
	isClient bool

	// streams 是活着的流
	streams map[uint32]*Stream

	// 正在拼的头块（HEADERS + CONTINUATION）
	pendingHeaders struct {
		streamID  uint32
		block     []byte
		endStream bool
		active    bool
	}

	// handler 是流事件的接收者（gRPC 之类）
	handler StreamHandler

	// settings
	peerMaxFrameSize  uint32
	peerHeaderTable   uint32
	peerInitialWindow uint32

	// goAway 收到过 GOAWAY
	goAway bool
	// goAwayCode
	goAwayCode ErrCode

	// lastStreamID 收到的最后一个流 ID（GOAWAY 时要用）
	lastStreamID uint32
}

// NewConn 建一条 HTTP/2 连接。
func NewConn(isClient bool, h StreamHandler) *Conn {
	c := &Conn{
		parser:       NewFrameParser(),
		dec:          newHpackDecoder(),
		encoder:      newHpackEncoder(),
		streams:      make(map[uint32]*Stream),
		handler:      h,
		isClient:     isClient,
		state:        stateOpen,
		nextStreamID: 1,
		// 对端的默认值（RFC 9113 6.5.2），SETTINGS 到了会改
		peerMaxFrameSize:  defaultMaxFrameSize,
		peerHeaderTable:   defaultHeaderTableSize,
		peerInitialWindow: defaultInitialWindowSize,
	}
	if !isClient {
		// 服务端要先等客户端的连接序言
		c.state = statePreface
		// 服务端发起的流是偶数
		c.nextStreamID = 2
	}
	return c
}

// defaultInitialWindowSize 是流的初始窗口（RFC 9113 6.9.2: 65535）。
const defaultInitialWindowSize = 65535

// SetHandler 换流事件的处理者（握手之后才知道要交给谁的时候用）。
func (c *Conn) SetHandler(h StreamHandler) { c.handler = h }

// ---------------------------------------------------------------------------
// 收

// Feed 喂一段从 fd 读到的字节，返回要发出去的数据。
//
// 非阻塞：不够一帧的字节留着，下次再喂。返回的切片是要写给对端的，
// 调用方写出去就行（可能是 nil）。
func (c *Conn) Feed(data []byte) ([]byte, error) {
	// 服务端先认连接序言
	if c.state == statePreface {
		n := len(data)
		if n > len(clientPreface) {
			n = len(clientPreface)
		}
		if len(data) < len(clientPreface) {
			// 还没收全，存着
			// TODO: 用一个 prefaceBuf 攒
			return nil, nil
		}
		if string(data[:len(clientPreface)]) != string(clientPreface) {
			return nil, fmt.Errorf("%w: bad client preface", ErrBadFrameHeader)
		}
		data = data[len(clientPreface):]
		c.state = stateSettings
	}

	_, err := c.parser.Parse(data, c.onFrame)
	if err != nil {
		// 协议错误要回 GOAWAY 再关
		return c.sendGoAway(ErrCodeProtocol, err.Error()), err
	}
	return c.takeOutput(), nil
}

// onFrame 处理一个解析出来的帧。
func (c *Conn) onFrame(f *Frame) error {
	// 连接级帧（流 0）先处理
	switch f.Type {
	case FrameSettings:
		return c.onSettings(f)
	case FramePing:
		return c.onPing(f)
	case FrameGoAway:
		return c.onGoAway(f)
	case FrameWindowUpdate:
		if f.StreamID == 0 {
			return c.onConnWindowUpdate(f)
		}
	case FramePriority:
		return nil // 我们不做优先级调度，收到就忽略
	case FramePushPromise:
		// 我们不发 PUSH，收到就是协议错误
		return fmt.Errorf("%w: PUSH_PROMISE not supported", ErrUnexpectedFrame)
	}

	if f.StreamID == 0 {
		return fmt.Errorf("%w: %v on stream 0", ErrUnexpectedFrame, f.Type)
	}
	if f.StreamID > c.lastStreamID {
		c.lastStreamID = f.StreamID
	}

	switch f.Type {
	case FrameHeaders:
		return c.onHeaders(f)
	case FrameContinuation:
		return c.onContinuation(f)
	case FrameData:
		return c.onData(f)
	case FrameRSTStream:
		return c.onRSTStream(f)
	case FrameWindowUpdate:
		return c.onStreamWindowUpdate(f)
	}
	return nil
}

func (c *Conn) onSettings(f *Frame) error {
	if f.Flags&FlagSettingsAck != 0 {
		return nil // 我们发的 SETTINGS 被确认了
	}

	for i := 0; i+6 <= len(f.Payload); i += 6 {
		id := uint16(f.Payload[i])<<8 | uint16(f.Payload[i+1])
		val := uint32(f.Payload[i+2])<<24 | uint32(f.Payload[i+3])<<16 |
			uint32(f.Payload[i+4])<<8 | uint32(f.Payload[i+5])

		switch id {
		case 0x1: // SETTINGS_HEADER_TABLE_SIZE
			c.peerHeaderTable = val
			// 我们编码时能用的动态表大小
			c.encoder.SetMaxSize(val)
		case 0x2: // SETTINGS_ENABLE_PUSH
			if val != 0 && val != 1 {
				return fmt.Errorf("%w: ENABLE_PUSH %d", ErrBadFrameLength, val)
			}
		case 0x3: // SETTINGS_MAX_CONCURRENT_STREAMS
			// 记下来但先不做并发限制
		case 0x4: // SETTINGS_INITIAL_WINDOW_SIZE
			if val > 1<<31-1 {
				return fmt.Errorf("%w: INITIAL_WINDOW_SIZE too large", ErrBadFrameLength)
			}
			c.peerInitialWindow = val
		case 0x5: // SETTINGS_MAX_FRAME_SIZE
			if val < defaultMaxFrameSize || val > MaxFrameSize {
				return fmt.Errorf("%w: MAX_FRAME_SIZE %d", ErrBadFrameLength, val)
			}
			c.peerMaxFrameSize = val
		case 0x6: // SETTINGS_MAX_HEADER_LIST_SIZE
			// 先不做限制
		default:
			// 未知 setting 要忽略（RFC 9113 6.5.3）
		}
	}

	// 必须回一个 ACK
	return c.writeFrame(AppendSettingsAck(nil))
}

func (c *Conn) onPing(f *Frame) error {
	if f.Flags&FlagPingAck != 0 {
		return nil // 是我们发出去的 ping 的回音
	}
	// 回 PING ACK，原样带上那 8 字节
	var data [8]byte
	copy(data[:], f.Payload)
	return c.writeFrame(AppendPing(nil, data, true))
}

func (c *Conn) onGoAway(f *Frame) error {
	c.goAway = true
	c.goAwayCode = ErrCode(uint32(f.Payload[4])<<24 | uint32(f.Payload[5])<<16 |
		uint32(f.Payload[6])<<8 | uint32(f.Payload[7]))
	c.state = stateClosed
	return nil
}

func (c *Conn) onConnWindowUpdate(f *Frame) error {
	inc := uint32(f.Payload[0])<<24 | uint32(f.Payload[1])<<16 |
		uint32(f.Payload[2])<<8 | uint32(f.Payload[3])
	if inc == 0 {
		return fmt.Errorf("%w: WINDOW_UPDATE increment 0", ErrBadFrameLength)
	}
	// 我们不做发送流控（先跑通，流控是后面的事）
	return nil
}

func (c *Conn) onStreamWindowUpdate(f *Frame) error {
	inc := uint32(f.Payload[0])<<24 | uint32(f.Payload[1])<<16 |
		uint32(f.Payload[2])<<8 | uint32(f.Payload[3])
	if inc == 0 {
		return fmt.Errorf("%w: stream WINDOW_UPDATE increment 0", ErrBadFrameLength)
	}
	return nil
}

func (c *Conn) onHeaders(f *Frame) error {
	payload := f.Payload
	var endStream bool

	if f.Flags&FlagHeadersPadded != 0 {
		if len(payload) == 0 {
			return fmt.Errorf("%w: HEADERS padded without pad length", ErrBadFrameLength)
		}
		padLen := int(payload[0])
		payload = payload[1:]
		if padLen > len(payload) {
			return fmt.Errorf("%w: HEADERS pad %d > %d", ErrBadFrameLength, padLen, len(payload))
		}
		payload = payload[:len(payload)-padLen]
	}
	if f.Flags&FlagHeadersPriority != 0 {
		if len(payload) < 5 {
			return fmt.Errorf("%w: HEADERS priority too short", ErrBadFrameLength)
		}
		payload = payload[5:] // 忽略优先级
	}

	endStream = f.Flags&FlagHeadersEndStream != 0

	if !f.EndHeaders() {
		// 还有 CONTINUATION，先攒着
		c.pendingHeaders.streamID = f.StreamID
		c.pendingHeaders.block = append(c.pendingHeaders.block[:0], payload...)
		c.pendingHeaders.endStream = endStream
		c.pendingHeaders.active = true
		return nil
	}
	return c.finishHeaders(f.StreamID, payload, endStream)
}

func (c *Conn) onContinuation(f *Frame) error {
	if !c.pendingHeaders.active || c.pendingHeaders.streamID != f.StreamID {
		return fmt.Errorf("%w: CONTINUATION on stream %d with nothing pending",
			ErrUnexpectedFrame, f.StreamID)
	}
	c.pendingHeaders.block = append(c.pendingHeaders.block, f.Payload...)
	if !f.EndHeaders() {
		return nil
	}
	block := c.pendingHeaders.block
	endStream := c.pendingHeaders.endStream
	c.pendingHeaders.active = false
	return c.finishHeaders(f.StreamID, block, endStream)
}

// finishHeaders 一个头块拼完了：解 HPACK，交给流。
func (c *Conn) finishHeaders(streamID uint32, block []byte, endStream bool) error {
	fields, err := c.dec.Decode(block)
	if err != nil {
		// HPACK 解错是连接级错误（动态表已经不可信了）
		return fmt.Errorf("%w: %v", errHPACKCompression, err)
	}

	s := c.stream(streamID)
	if s == nil {
		return fmt.Errorf("%w: HEADERS on stream %d that is not open",
			ErrUnexpectedFrame, streamID)
	}
	s.headers = append(s.headers[:0], fields...)

	if c.handler != nil {
		c.handler.OnHeaders(c, streamID, s.headers, endStream)
	}
	if endStream {
		s.remoteEnded = true
	}
	return nil
}

func (c *Conn) onData(f *Frame) error {
	payload := f.Payload
	if f.Flags&FlagDataPadded != 0 {
		if len(payload) == 0 {
			return fmt.Errorf("%w: DATA padded without pad length", ErrBadFrameLength)
		}
		padLen := int(payload[0])
		payload = payload[1:]
		if padLen > len(payload) {
			return fmt.Errorf("%w: DATA pad %d > %d", ErrBadFrameLength, padLen, len(payload))
		}
		payload = payload[:len(payload)-padLen]
	}

	s := c.stream(f.StreamID)
	if s == nil {
		return fmt.Errorf("%w: DATA on stream %d that is not open",
			ErrUnexpectedFrame, f.StreamID)
	}
	endStream := f.Flags&FlagDataEndStream != 0

	if c.handler != nil && len(payload) > 0 {
		c.handler.OnData(c, f.StreamID, payload, endStream)
	}
	if endStream {
		s.remoteEnded = true
	}
	return nil
}

func (c *Conn) onRSTStream(f *Frame) error {
	code := ErrCode(uint32(f.Payload[0])<<24 | uint32(f.Payload[1])<<16 |
		uint32(f.Payload[2])<<8 | uint32(f.Payload[3]))
	if c.handler != nil {
		c.handler.OnRSTStream(c, f.StreamID, code)
	}
	delete(c.streams, f.StreamID)
	return nil
}

// ---------------------------------------------------------------------------
// 流

// stream 取一个流，没有就建（对端发起的就是这样）。
func (c *Conn) stream(id uint32) *Stream {
	s := c.streams[id]
	if s == nil {
		s = &Stream{ID: id, state: StreamOpen}
		c.streams[id] = s
	}
	return s
}

// Stream 是一个 HTTP/2 流。
type Stream struct {
	ID      uint32
	state   StreamState
	headers []HeaderField

	// remoteEnded 对端发了 END_STREAM
	remoteEnded bool
	// localEnded 我们发了 END_STREAM
	localEnded bool

	// sendWindow 是我们还能往这个流上发多少字节
	sendWindow int32
}

// StreamState 是流的状态（RFC 9113 5.1 的状态机）。
type StreamState uint8

const (
	StreamIdle StreamState = iota
	StreamOpen
	StreamHalfClosedRemote
	StreamHalfClosedLocal
	StreamClosed
)

// GetStream 取一个流。
func (c *Conn) GetStream(id uint32) *Stream { return c.streams[id] }

// CloseStream 关掉一个流（发完响应之后调）。
func (c *Conn) CloseStream(id uint32) { delete(c.streams, id) }

// ---------------------------------------------------------------------------
// 发

// NewStreamID 分配一个新的流 ID。
func (c *Conn) NewStreamID() uint32 {
	id := c.nextStreamID
	c.nextStreamID += 2 // 同方向的流 ID 要跳 2（保持奇偶性）
	return id
}

// WriteHeaders 发一个 HEADERS 帧。
func (c *Conn) WriteHeaders(streamID uint32, fields []HeaderField, endStream bool) error {
	block, err := c.encoder.Encode(fields)
	if err != nil {
		return err
	}
	// 头块比分片上限大要拆成 HEADERS + CONTINUATION
	return c.writeHeaderBlock(streamID, block, endStream)
}

// writeHeaderBlock 把头块按分片上限拆开发。
//
// RFC 9113 4.3：头块超过 SETTINGS_MAX_FRAME_SIZE 要拆成 HEADERS +
// 若干 CONTINUATION，中间不能夹别的帧（所以这里一次全发出去）。
func (c *Conn) writeHeaderBlock(streamID uint32, block []byte, endStream bool) error {
	max := int(c.peerMaxFrameSize)
	first := len(block) <= max

	if first {
		return c.writeFrame(AppendHeaders(nil, streamID, block, endStream, true))
	}

	buf := AppendHeaders(nil, streamID, block[:max], endStream, false)
	block = block[max:]
	for len(block) > 0 {
		n := max
		if len(block) < n {
			n = len(block)
		}
		last := n == len(block)
		buf = AppendFrameHeader(buf, FrameContinuation, boolToFlag(last, FlagContinuationEndHeaders), streamID, n)
		buf = append(buf, block[:n]...)
		block = block[n:]
	}
	return c.writeFrame(buf)
}

func boolToFlag(b bool, f uint8) uint8 {
	if b {
		return f
	}
	return 0
}

// WriteData 发一个 DATA 帧。
func (c *Conn) WriteData(streamID uint32, data []byte, endStream bool) error {
	max := int(c.peerMaxFrameSize)
	if len(data) <= max {
		return c.writeFrame(AppendData(nil, streamID, data, endStream))
	}
	// 拆成多帧，最后一帧带 END_STREAM
	buf := make([]byte, 0, len(data)+frameHeaderLen*2)
	for len(data) > 0 {
		n := max
		if len(data) < n {
			n = len(data)
		}
		last := n == len(data)
		buf = AppendData(buf, streamID, data[:n], last && endStream)
		data = data[n:]
	}
	return c.writeFrame(buf)
}

// sendGoAway 发 GOAWAY。
func (c *Conn) sendGoAway(code ErrCode, debug string) []byte {
	c.state = stateClosed
	c.goAway = true
	buf := AppendGoAway(nil, c.lastStreamID, uint32(code), []byte(debug))
	c.mu.Lock()
	c.outBuf = append(c.outBuf, buf...)
	out := c.outBuf
	c.outBuf = nil
	c.mu.Unlock()
	return out
}

// writeFrame 把一个帧追加到待发缓冲区。
func (c *Conn) writeFrame(b []byte) error {
	c.mu.Lock()
	c.outBuf = append(c.outBuf, b...)
	c.mu.Unlock()
	return nil
}

// takeOutput 取走待发的数据。
func (c *Conn) takeOutput() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.outBuf) == 0 {
		return nil
	}
	out := c.outBuf
	c.outBuf = nil
	return out
}

// TakeOutput 取走待发的数据（给事件循环用）。
func (c *Conn) TakeOutput() []byte { return c.takeOutput() }

// ---------------------------------------------------------------------------
// 便捷方法
//
// 这几个是"已经拼好的帧直接返回给你"的写法，给两种场景用：
//   - 一次性把请求发出去（发完就走）
//   - 测试里拼数据
//
// 事件循环那边更常用的是 WriteHeaders / WriteData（写到内部缓冲，
// 之后 TakeOutput 取走）。

// MustHeaders 拼一个 HEADERS 帧并返回字节。
//
// 名字里的 Must 是"要么成功要么 panic"：编码失败只可能是动态表满了这种
// 我们自己状态的问题，不是调用方传错了参数。
func (c *Conn) MustHeaders(streamID uint32, fields []HeaderField, endStream bool) []byte {
	block, err := c.encoder.Encode(fields)
	if err != nil {
		panic("http2: encode headers: " + err.Error())
	}
	var buf []byte
	buf = AppendHeaders(buf, streamID, block, endStream, true)
	return buf
}

// MustData 拼 DATA 帧（超过分片上限会拆成多个）。载荷全部拷贝进返回值，
// 调用方可以立刻复用原来的切片。
func (c *Conn) MustData(streamID uint32, data []byte, endStream bool) []byte {
	max := int(c.peerMaxFrameSize)
	var buf []byte
	for len(data) > 0 {
		n := max
		if len(data) < n {
			n = len(data)
		}
		last := n == len(data)
		buf = AppendData(buf, streamID, data[:n], last && endStream)
		data = data[n:]
	}
	return buf
}

// GoAway 收到过 GOAWAY 吗。
func (c *Conn) GoAway() bool { return c.goAway }

// GoAwayCode GOAWAY 的错误码。
func (c *Conn) GoAwayCode() ErrCode { return c.goAwayCode }

// PeerMaxFrameSize 对端声明的最大帧大小。
func (c *Conn) PeerMaxFrameSize() uint32 { return c.peerMaxFrameSize }

var errUnused = errors.New("unused")
