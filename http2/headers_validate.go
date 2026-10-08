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
	"strconv"
	"strings"
)

// 请求头的合法性校验（RFC 9113 8.3）。
//
// **这些全是流级错误**（PROTOCOL_ERROR 的 RST_STREAM）：一个请求的头
// 写错了，是那个请求的事，同一条连接上别的流不受影响——这正是 HTTP/2
// 多路复用的意义。要是因为一个坏请求就把整条连接 GOAWAY 掉，那上面
// 所有正在跑的请求（浏览器一次开几十个）全被连累。
//
// 校验的是"HTTP/2 比 HTTP/1.1 更严"的那几条：
//
//	伪头（:method 这些）
//	  - 必须全在普通头**之前**
//	  - 不能重复
//	  - 请求要有 :method，CONNECT 之外要有 :scheme 和 :path
//	  - 不能出现只属于响应的伪头（:status）
//	  - 不能有不认识的伪头
//
//	普通头
//	  - 名字必须小写（HTTP/2 要求全小写，大写是 PROTOCOL_ERROR）
//	  - 不能有 connection-specific 的（Connection、Keep-Alive、
//	    Transfer-Encoding……HTTP/2 用自己的方式管连接，这些是 1.1 的东西）
//	  - TE 只允许 "trailers"
//
// 为什么这些"严格"很重要：HTTP/1.1 和 HTTP/2 之间要做转换（代理、
// 网关），如果 HTTP/2 这边不校验，同一个请求在两种协议下能被解读成
// 不同的意思——那是**请求走私**的经典手法。规范要求严格校验就是为了
// 堵死这条路。

// validateRequestHeaders 校验一个请求头块。
//
// 返回 (是否是 trailer, 错误描述)。错误描述为空表示合法。
//
// **要求伪头齐全**（:method/:scheme/:path）——这是**服务端**该做的检查：
// 对端发来的是一个完整请求，缺了哪个它自己就说不清要请求什么。
//
// 反过来，如果我们只是被动地解一段头（测试、客户端看响应），就不该
// 要求这些——响应头里根本不会有 :method。所以这个函数只管"完整的
// 请求"这一种；别的场景用 validateHeaderSyntax。
func validateRequestHeaders(fields []HeaderField) (isTrailer bool, errMsg string) {
	isTrailer, errMsg, hasMethod, hasScheme, hasPath, hasStatus := validateHeaderSyntax(fields)
	if errMsg != "" {
		return isTrailer, errMsg
	}
	_ = isTrailer

	// **请求里不能有 :status**——那是响应的伪头，出现说明对端把方向搞混了
	if hasStatus {
		return false, ":status in a request"
	}

	// 必需伪头的检查（RFC 9113 8.3.1）
	// CONNECT 是特例（没有 :scheme/:path，用 :authority），我们不支持
	if !hasMethod {
		return false, "missing :method"
	}
	if !hasScheme {
		return false, "missing :scheme"
	}
	if !hasPath {
		return false, "missing :path"
	}
	return false, ""
}

// validateHeaderSyntax 只校验**语法**，不要求伪头齐全。
//
// 用于"不一定是完整请求"的场景（单独解一段头块）。语法错误的危害和上面
// 说的一样（大小写、连接相关字段、伪头位置），所以这几条必须查。
//
// 返回 (是否 trailer, 错误描述, 见到 :method/:scheme/:path 没有)。
func validateHeaderSyntax(fields []HeaderField) (isTrailer bool, errMsg string, hasMethod, hasScheme, hasPath, hasStatus bool) {
	seenRegular := false
	seenPseudo := make(map[string]bool, 4)

	for _, f := range fields {
		name := f.Name

		if len(name) == 0 {
			return false, "empty header name", false, false, false, false
		}

		// **大写字母是非法的**（RFC 9113 8.2.1）。
		//
		// 为什么这么严：HTTP/1.1 的头名大小写无关，而 HPACK 是大小写
		// 敏感的。这里放过大写的话，一个 "Content-Length" 到了 HTTP/1.1
		// 那边变成 "content-length"，中间做协议转换的时候可能出现两个
		// "同一个"头——那是请求走私的经典入口（RFC 9113 8.2.1 专门讲了）。
		for i := 0; i < len(name); i++ {
			if name[i] >= 'A' && name[i] <= 'Z' {
				return false, "header name contains uppercase: " + name, false, false, false, false
			}
		}

		if name[0] == ':' {
			// 伪头
			if seenRegular {
				// 伪头必须在所有普通头之前（RFC 9113 8.3）
				return false, "pseudo-header " + name + " after regular headers", false, false, false, false
			}
			if seenPseudo[name] {
				return false, "duplicate pseudo-header " + name, false, false, false, false
			}
			seenPseudo[name] = true

			switch name {
			case ":method":
				hasMethod = true
			case ":scheme":
				hasScheme = true
			case ":path":
				if f.Value == "" {
					// **:path 不能是空串**（RFC 9113 8.3.1）——CONNECT 除外
					return false, "empty :path", false, false, false, false
				}
				hasPath = true
			case ":authority":
				// 合法，随便什么值
			case ":status":
				// :status 是**响应**的伪头。声明出来，由调用方按方向
				// 判断合不合法——请求里出现它是错的（对端把请求响应
				// 搞混了），响应里出现它是必须的。
				hasStatus = true
			case ":protocol":
				// 扩展 CONNECT 用的，我们不支持
				return false, "unsupported pseudo-header :protocol", false, false, false, false
			default:
				// 不认识的伪头必须报错（RFC 9113 8.3）：伪头是扩展点，
				// 放过去的话以后协议加了新伪头，老实现会把它当普通头，
				// 两边理解不一致。
				return false, "unknown pseudo-header " + name, false, false, false, false
			}
			continue
		}

		// 普通头
		seenRegular = true

		switch name {
		case "connection", "keep-alive", "proxy-connection",
			"transfer-encoding", "upgrade":
			// **connection-specific 的头在 HTTP/2 里是禁止的**
			// （RFC 9113 8.2.2）。HTTP/2 有自己的连接管理（SETTINGS、
			// GOAWAY、流），把这些 1.1 的概念搬过来只会造成歧义：
			// "Connection: keep-alive" 到底听谁的？
			return false, "connection-specific header: " + name, false, false, false, false
		case "te":
			// TE 只允许 "trailers"（RFC 9113 8.2.2）。别的值在 1.1 里
			// 可能是合法的（TE: gzip），但在 2 里没有对应的语义。
			if strings.TrimSpace(f.Value) != "trailers" {
				return false, "TE with value other than trailers: " + f.Value, false, false, false, false
			}
		}
	}

	return false, "", hasMethod, hasScheme, hasPath, hasStatus
}

// contentLengthOf 从请求头里取 content-length。
//
// 返回 (值, 有没有, 错误描述)。
//
// **多条 content-length 是错的**（RFC 9113 8.1.1）：HTTP/1.1 里"多个
// Content-Length"曾经被用来做请求走私（中间设备和后端各取一个，两个
// 数字不一样，后面的字节就被解读成不同的请求）。HTTP/2 直接把它定成
// 非法——一条就够，多了就是可疑。
//
// 值不是合法的非负整数也拒。前后有空白先去掉（HTTP/1.1 允许
// "Content-Length: 42 "这种，转换过来的时候会带空白）。
func contentLengthOf(fields []HeaderField) (val int64, ok bool, errMsg string) {
	seen := false
	for _, f := range fields {
		if f.Name != "content-length" {
			continue
		}
		if seen {
			return 0, false, "duplicate content-length"
		}
		seen = true

		s := strings.TrimSpace(f.Value)
		if s == "" {
			return 0, false, "empty content-length"
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return 0, false, "malformed content-length: " + f.Value
		}
		val = n
	}
	return val, seen, ""
}
