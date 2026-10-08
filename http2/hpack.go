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

	"golang.org/x/net/http2/hpack"
)

// HPACK（RFC 7541）：HTTP/2 的头压缩，用 golang.org/x/net/http2/hpack。
//
// 为什么 HTTP/2 要压头：一个请求的 header 动辄几百字节，而 HTTP/2 上一个
// 连接会跑成千上万个请求，同一个客户端的头几乎都一样（User-Agent、
// Accept、Cookie...）。HPACK 用一个**两边同步维护的索引表**——发过的头
// 记进表里，下次只发一个索引号。
//
// **为什么用官方实现而不是自己写**：HPACK 有状态，而且 Huffman 码表是
// 257 项（RFC 7541 附录 B），手工抄一遍很容易错一位——错一位的症状是
// "某些头偶尔解错"，在压测里根本看不出来，在生产里就是偶发的 400。
// x/net/http2/hpack 是官方维护的，边界情况（表溢出、Huffman 填充位、
// 变长整数溢出）都过过测试。
//
// 我们这边要做的只是"每次连接一个解码器"——HPACK 的编解码器都是
// 有状态的（动态表跨头块保留），**不能跨连接共用**。

// HeaderField 是一个解出来的头。
type HeaderField = hpack.HeaderField

// hpackDecoder 包一层 x/net 的解码器，把它那个回调式的接口变成
// "喂一个头块，拿一组头"。
//
// 官方那个是流式的（每解出一个头就回调一次），因为我们一次拿到的是
// 一整个头块（HEADERS + CONTINUATION 拼起来的），所以攒一下再返回。
type hpackDecoder struct {
	dec *hpack.Decoder
	// cur 是这次解码收上来的头
	cur []HeaderField
	// err 是回调里遇到的错误
	err error
}

func newHpackDecoder() *hpackDecoder {
	d := &hpackDecoder{}
	d.dec = hpack.NewDecoder(defaultHeaderTableSize, d.onField)
	return d
}

// defaultHeaderTableSize 是动态表的默认上限（RFC 7541 4.2: 4096 字节）。
const defaultHeaderTableSize = 4096

func (d *hpackDecoder) onField(f HeaderField) {
	if d.err != nil {
		return
	}
	// 动态表爆了会从这儿报出来
	d.cur = append(d.cur, f)
}

// SetMaxSize 改动态表上限（对端 SETTINGS 声明之后调）。
func (d *hpackDecoder) SetMaxSize(n uint32) {
	d.dec.SetMaxDynamicTableSize(n)
}

// Decode 解一个头块，返回解出来的头。
//
// 返回的切片在下一次 Decode 之前有效（复用同一块）。
func (d *hpackDecoder) Decode(block []byte) ([]HeaderField, error) {
	d.cur = d.cur[:0]
	d.err = nil

	if _, err := d.dec.Write(block); err != nil {
		return nil, err
	}
	// **Close 是必须的**，不是"清理资源"那种可选的。
	//
	// x/net 的解码器是流式的：Write 遇到"这段还不够解出一个字段"时会
	// 把字节留在内部缓冲里、返回一个 nil 错误（意思是"再多给点"）。
	// 但**一个头块到这里就结束了**（HEADERS/CONTINUATION 带了
	// END_HEADERS），不该再等——还留着没解完的字节就是**块本身是坏的**
	// （截断的、或者 Huffman 编码里有非法符号）。
	//
	// Close 就是干这个的：没解完就报 "truncated headers"。不调它的话，
	// 那些坏块会被当成"合法的空头块"放过去——实测：h2spec 的
	// "Huffman 里含 EOS 符号"那条用例，我们回的是正常响应而不是
	// COMPRESSION_ERROR 的 GOAWAY。
	if err := d.dec.Close(); err != nil {
		return nil, err
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.cur, nil
}

// ---------------------------------------------------------------------------
// 编码

// hpackEncoder 包一层 x/net 的编码器。
type hpackEncoder struct {
	buf bytes.Buffer
	enc *hpack.Encoder
}

func newHpackEncoder() *hpackEncoder {
	e := &hpackEncoder{}
	e.enc = hpack.NewEncoder(&e.buf)
	return e
}

// Encode 把一组头编成头块。
//
// 返回的切片在下一次 Encode 之前有效。
func (e *hpackEncoder) Encode(fields []HeaderField) ([]byte, error) {
	e.buf.Reset()
	for _, f := range fields {
		if err := e.enc.WriteField(f); err != nil {
			return nil, err
		}
	}
	return e.buf.Bytes(), nil
}

// SetMaxSize 改动态表上限（我们自己在 SETTINGS 里声明之后调）。
func (e *hpackEncoder) SetMaxSize(n uint32) {
	e.enc.SetMaxDynamicTableSize(n)
}

var errHPACKCompression = errors.New("http2: HPACK compression error")
