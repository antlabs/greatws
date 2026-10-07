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

package grpc

import (
	"strings"
	"testing"

	"github.com/antlabs/fio/http2"
)

func TestStatusRoundTrip(t *testing.T) {
	for _, want := range []*Status{
		{Code: OK},
		{Code: NotFound, Message: "user 42 not found"},
		{Code: InvalidArgument, Message: "bad input"},
		{Code: Internal, Message: "中文错误信息"},
		{Code: DeadlineExceeded, Message: "line1\nline2"},
	} {
		fields := AppendStatus(nil, want)
		got := StatusFromHeaders(fields)
		if got.Code != want.Code {
			t.Errorf("Code = %v, want %v", got.Code, want.Code)
		}
		if got.Message != want.Message {
			t.Errorf("Message = %q, want %q", got.Message, want.Message)
		}
	}
}

// 没有 grpc-status 的 trailer 按 Unknown 处理（规范要求必须有）。
func TestStatusMissing(t *testing.T) {
	got := StatusFromHeaders([]http2.HeaderField{
		{Name: "content-type", Value: "application/grpc"},
	})
	if got.Code != Unknown {
		t.Errorf("Code = %v, want Unknown", got.Code)
	}
}

// grpc-status 不是数字的话也要按 Unknown 处理，不能 panic。
func TestStatusMalformed(t *testing.T) {
	got := StatusFromHeaders([]http2.HeaderField{
		{Name: "grpc-status", Value: "not-a-number"},
	})
	if got.Code != Unknown {
		t.Errorf("Code = %v, want Unknown", got.Code)
	}
}

// content-type 判断要用前缀匹配（application/grpc+proto 也算）。
func TestContentType(t *testing.T) {
	for _, c := range []struct {
		v    string
		want bool
	}{
		{"application/grpc", true},
		{"application/grpc+proto", true},
		{"application/grpc+json", true},
		{"application/grpc-web", true},
		{"application/json", false},
		{"text/plain", false},
		{"", false},
		{"application/gr", false},
	} {
		if got := IsGRPCContentType(c.v); got != c.want {
			t.Errorf("IsGRPCContentType(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

// 错误信息里的特殊字节要能原样来回（HTTP/2 头值只放得下可见 ASCII）。
func TestMessageEncoding(t *testing.T) {
	for _, msg := range []string{
		"simple",
		"with space",
		"中文",
		"percent %20 already",
		"tab\there",
		"newline\nhere",
		"\x00\x01\x02 binary",
	} {
		s := &Status{Code: Internal, Message: msg}
		fields := AppendStatus(nil, s)
		got := StatusFromHeaders(fields)
		if got.Message != msg {
			t.Errorf("消息 %q 往返之后变成 %q", msg, got.Message)
		}
	}
}

// 编码之后的值必须是能放进 HTTP/2 头的（可见 ASCII，不含控制字符）。
func TestEncodedValueIsHeaderSafe(t *testing.T) {
	s := &Status{Code: Internal, Message: "带\n换行\t和中文"}
	fields := AppendStatus(nil, s)
	for _, f := range fields {
		for i := 0; i < len(f.Value); i++ {
			c := f.Value[i]
			if c < 0x20 || c > 0x7e {
				t.Fatalf("头值 %q 里有不能放的字节 %#x", f.Value, c)
			}
		}
	}
}

// Code 的字符串形式要和 grpc-go 一致（日志里好认）。
func TestCodeString(t *testing.T) {
	for _, c := range []struct {
		code Code
		want string
	}{
		{OK, "OK"},
		{NotFound, "NotFound"},
		{Unavailable, "Unavailable"},
		{Unauthenticated, "Unauthenticated"},
		{Code(99), "Code(99)"},
	} {
		if got := c.code.String(); got != c.want {
			t.Errorf("%d.String() = %q, want %q", c.code, got, c.want)
		}
	}
}

// Status 实现 error 接口，OK 的时候 Error() 是空串。
func TestStatusError(t *testing.T) {
	ok := &Status{Code: OK}
	if ok.Error() != "" {
		t.Errorf("OK 的 Error() = %q, 应该是空串", ok.Error())
	}
	notFound := &Status{Code: NotFound, Message: "no such user"}
	if !strings.Contains(notFound.Error(), "NotFound") ||
		!strings.Contains(notFound.Error(), "no such user") {
		t.Errorf("Error() = %q", notFound.Error())
	}
}
