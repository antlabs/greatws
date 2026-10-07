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

package http

import (
	"errors"

	"github.com/antlabs/httparser"
)

// Parser 是一条连接上的 HTTP/1.1 请求解析状态。
//
// 它包着 httparser.Parser 和那个攒 Request 的 builder，对调用方暴露的
// 契约和 httparser 一致：
//
//	var p = NewParser(0)
//	for {
//	    n, err := p.Parse(buf)   // buf 是"上次没消化的 + 这次新读的"
//	    if err != nil { ... }
//	    if p.Done() { req := p.Request(); break }
//	    buf = buf[n:]            // 没消化的留下来，下次接着喂
//	}
//
// **关键**：没消化的字节要留着，不能丢。httparser 是按字节推进的状态机，
// 一个 token 没读到分隔符（比如方法名后面的空格还没来）它就不消费那个
// 字节，返回 0——这时候把数据丢掉，剩下那半个方法名就再也拼不起来了。
// 调用方每次 buf = buf[n:]，再把新读到的 append 在后面。
//
// 这个包不自己缓冲：攒的活留给调用方（它本来就有那个读缓冲区），这里
// 只推进状态。缓冲整条报文是阻塞式实现的做法，会把事件循环卡住。
//
// 一次 Parse 里最多只出一条报文：Done 之后调用方先取走请求，Reset 再
// 接着喂剩下的（keep-alive 的下一个）。
type Parser struct {
	p   *httparser.Parser
	b   *requestBuilder
	set *httparser.Setting

	// done 是"这条报文解完了"。MessageComplete 回调里置上。
	//
	// 为什么不用 httparser 的 EOF(): 它对 REQUEST 无条件返回 true（见
	// httparser 的 parser.go: "if p.hType == REQUEST { return true }"），
	// 当不了完成信号。真正可靠的是 MessageComplete 这个回调，所以这里
	// 自己记。
	done bool

	// endOffset 是这条报文在 buf 里的结束位置，MessageComplete 回调给。
	//
	// **这个才是权威的消化量。** httparser 的 Execute 会把整个 buf 里
	// 的所有报文一次解完（没有"解一条就停"的开关），返回值是它解完所有
	// 报文的位置。keep-alive 的连接上，一个 buf 里可能有多个请求——用
	// Execute 的返回值当"第一条请求结束的位置"，第二条会被当成第一条的
	// 数据（实测 Target 变成 "/a/b"）。
	//
	// offset 是**报文最后一个字节的下标**（httparser 的 complete 传的是
	// 当前字节的 i，不是下一个字节的位置），所以消化量是 offset+1。
	// 实测: "GET /a HTTP/1.1\r\nHost: x\r\n\r\n" 长 28 字节，
	// 回调给 off=27。
	endOffset int

	// upgrade 是报文里有 Upgrade（websocket 握手就是这条）。
	upgrade bool

	maxHeaderSize int32
}

// DefaultMaxHeaderSize 是没显式设置时的头上限。
//
// 和 httparser 的默认值一致（8KB）。真实请求头一般不到 4KB，8KB 够用。
const DefaultMaxHeaderSize = 8 * 1024

var (
	// ErrHeaderTooLarge 头超过 maxHeaderSize。调用方该回 431。
	ErrHeaderTooLarge = errors.New("http: header too large")
	// ErrBadMessage 报文不是合法 HTTP/1.1。调用方该回 400。
	ErrBadMessage = errors.New("http: malformed message")
)

// NewParser 建一个请求解析器。maxHeaderSize <= 0 时用 DefaultMaxHeaderSize。
func NewParser(maxHeaderSize int32) *Parser {
	if maxHeaderSize <= 0 {
		maxHeaderSize = DefaultMaxHeaderSize
	}
	hp := httparser.New(httparser.REQUEST)
	hp.MaxHeaderSize = maxHeaderSize

	p := &Parser{
		p:             hp,
		maxHeaderSize: maxHeaderSize,
	}
	p.b = newRequestBuilder(p)
	p.set = p.b.setting(p)
	return p
}

// Parse 喂一段字节，返回第一条报文消化了多少。
//
// 返回的 n < len(buf) 说明一条报文已经解完，剩下的字节属于下一个请求
// （keep-alive），调用方自己留着，Reset 之后接着喂。
//
// n 用的是 MessageComplete 回调给的报文边界，不是 Execute 的返回值：
// 后者会把整个 buf 里的报文都解完（见 endOffset 的注释）。
func (p *Parser) Parse(buf []byte) (int, error) {
	if p.done {
		// 上一条还没被取走，不该继续喂
		return 0, ErrBadMessage
	}

	n, err := p.p.Execute(p.set, buf)
	if err != nil {
		return n, errors.Join(ErrBadMessage, err)
	}
	if !p.done {
		// 报文还没收全。把 httparser 消化掉的那部分切掉，没消化的留给
		// 调用方——它可能是"半个方法名"（httparser 要等空格才知道方法名
		// 是什么，那之前一个字节都不消费，返回 0），也可能是解到一半的
		// 中间状态。丢掉的话剩下的字节再也拼不起来。
		return n, nil
	}
	// 解完了：用报文边界（endOffset），不是 Execute 的返回值——keep-alive
	// 的一个 buf 里可能有第二个请求，Execute 会连它一起解了。
	return p.endOffset + 1, nil
}

// Done 这条报文解完了没有。
func (p *Parser) Done() bool { return p.done }

// Upgrade 报文是不是一个 Upgrade 请求（websocket 握手）。
func (p *Parser) Upgrade() bool { return p.upgrade }

// Parser 暴露 httparser 的解析器，给需要看版本号这些字段的调用方。
func (p *Parser) Parser() *httparser.Parser { return p.p }

// Request 返回解好的请求。没解完时返回 nil。
//
// 返回的 Request 在下一次 Reset 之前一直有效；Body 是攒出来的副本，
// 调用方可以一直拿着。
func (p *Parser) Request() *Request {
	if !p.done {
		return nil
	}
	return p.b.req
}

// Reset 准备解析下一个请求（keep-alive）。
func (p *Parser) Reset() {
	p.p.Reset()
	p.p.MaxHeaderSize = p.maxHeaderSize
	p.b.reset()
	p.done = false
	p.upgrade = false
	p.endOffset = 0
}
