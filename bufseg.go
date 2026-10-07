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

import "github.com/antlabs/wsutil/bytespool"

// bufseg 是一块"把帧头和 payload 拼在一起"用的临时缓冲。
//
// 小消息(len(header)+len(payload) <= 4KB)走的是"拼成一块 + sendto"这条路:
// sendto 只要一个指针, sendmsg 要读 iovec 数组, 小消息下前者更便宜。
// fnet 的 writeFrame 就是这个阈值(maxCopiedPayload = 4KB), 超过才用 writev。
//
// 4KB 以内用栈上的数组, 不碰堆也不碰池; 超过才从池里取。这样 1024B 这种
// 最常见的消息类型完全没有分配。
type bufseg struct {
	stack [4096]byte
	heap  *[]byte
}

//go:noinline
func (s *bufseg) alloc(headerLen, payloadLen int) []byte {
	n := headerLen + payloadLen
	if n <= len(s.stack) {
		return s.stack[:n]
	}
	s.heap = bytespool.GetBytes(n)
	return (*s.heap)[:n]
}

//go:noinline
func (s *bufseg) free() {
	if s.heap != nil {
		bytespool.PutBytes(s.heap)
		s.heap = nil
	}
}
