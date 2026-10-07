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

import "encoding/binary"

// wsHeader 在栈上拼 websocket 帧头, 返回实际长度。
//
// 只处理服务端(不需要掩码)的常见情况: payload <= 125 用 2 字节, <= 65535
// 用 4 字节。客户端和超长消息退回 WriteFrame(那两条路不是热路径)。
func wsHeader(buf []byte, op uint8, payloadLen int) int {
	buf[0] = 0x80 | (op & 0x0F) // FIN + opcode
	switch {
	case payloadLen <= 125:
		buf[1] = byte(payloadLen)
		return 2
	case payloadLen <= 65535:
		buf[1] = 126
		buf[2] = byte(payloadLen >> 8)
		buf[3] = byte(payloadLen)
		return 4
	}
	buf[1] = 127
	binary.BigEndian.PutUint64(buf[2:], uint64(payloadLen))
	return 10
}
