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

import "fmt"

// HTTP/2 的错误分**两级**，这个区别是协议的核心设计之一
// （RFC 9113 5.4）：
//
//	连接错误（connection error）
//	  -> 整条连接废掉：发 GOAWAY 然后关
//	  -> 用于"这条连接没法继续信任了"：帧格式错、HPACK 解不了、
//	     流 ID 乱来、SETTINGS 非法……
//
//	流错误（stream error）
//	  -> 只废掉那一个流：发 RST_STREAM，连接照常用
//	  -> 用于"这一个请求有问题"：流状态不对、头部不合法、
//	     流控超了……同一个连接上别的流不受影响
//
// 搞错这一级会怎么样：
//   - 该报流错误却 GOAWAY：一个坏请求把整个连接上所有流都杀了
//     （HTTP/2 的多路复用就白做了）
//   - 该报连接错误却只 RST_STREAM：连接进入一个双方理解不一致的
//     状态，后面全是错的
//
// h2spec 有大量用例专门测这个——它检查的就是"你回的是哪一个"。

// ConnError 是连接级错误：发 GOAWAY 然后关。
type ConnError struct {
	Code ErrCode
	Msg  string
}

func (e *ConnError) Error() string {
	return fmt.Sprintf("http2: connection error %s: %s", e.Code, e.Msg)
}

// connError 造一个连接错误（内部用，调用方不用关心错误的具体类型，
// 只关心"这该走哪条路"）。
func connError(code ErrCode, format string, args ...any) error {
	return &ConnError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// StreamError 是流级错误：发 RST_STREAM，连接继续用。
type StreamError struct {
	StreamID uint32
	Code     ErrCode
	Msg      string
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("http2: stream %d error %s: %s", e.StreamID, e.Code, e.Msg)
}

// streamError 造一个流错误。
func streamError(streamID uint32, code ErrCode, format string, args ...any) error {
	return &StreamError{StreamID: streamID, Code: code, Msg: fmt.Sprintf(format, args...)}
}

// asConnError 把任意错误当成连接错误看。
//
// **默认归类是"连接错误"**：一个我们不认识的错误，说明有什么地方没按
// 预期走——这时候继续用这条连接是危险的（双方状态可能已经不一致了）。
// 反过来把连接错误当成流错误只是"少杀了一点"，把流错误当成连接错误
// 则是"多杀了一大片"。默认取严格的那个。
func asConnError(err error) *ConnError {
	if ce, ok := err.(*ConnError); ok {
		return ce
	}
	// 已经是流错误的话，包一层连接错误说明"这个不该是流级的"
	if se, ok := err.(*StreamError); ok {
		return &ConnError{Code: se.Code, Msg: se.Msg}
	}
	return &ConnError{Code: ErrCodeProtocol, Msg: err.Error()}
}

// asStreamError 取流错误的流 ID 和错误码。
func asStreamError(err error) (*StreamError, bool) {
	se, ok := err.(*StreamError)
	return se, ok
}
