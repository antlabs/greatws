// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package greatws

import (
	"time"

	"github.com/antlabs/wsutil/enum"
)

type Config struct {
	Callback
	tcpNoDelay                      bool              //TODO: 加下这个功能
	replyPing                       bool              // 开启自动回复
	decompression                   bool              // 开启解压缩功能
	compression                     bool              // 开启压缩功能
	ignorePong                      bool              // 忽略pong消息
	disableBufioClearHack           bool              // 关闭bufio的clear hack优化
	utf8Check                       func([]byte) bool // utf8检查
	readTimeout                     time.Duration     // 加下这个功能
	windowsMultipleTimesPayloadSize float32           // 设置几倍(1024+14)的payload大小
	maxDelayWriteNum                int32             // 最大延迟包的个数, 默认值为10
	delayWriteInitBufferSize        int32             // 延迟写入的初始缓冲区大小, 默认值是8k
	maxDelayWriteDuration           time.Duration     // 最大延迟时间, 默认值是10ms
	subProtocols                    []string          // 设置支持的子协议
	multiEventLoop                  *MultiEventLoop   // 事件循环
	callbackInEventLoop             bool              // 在event loop中运行websocket OnOpen, OnMessage, OnClose 回调函数
	runInGoStrategy                 taskStrategy      //运行业务OnMessage的策略
}

// func (c *Config) useIoUring() bool {
// 	return c.multiEventLoop.flag == EVENT_IOURING
// }

func (c *Config) initPayloadSize() int {
	return int((1024.0 + float32(enum.MaxFrameHeaderSize)) * c.windowsMultipleTimesPayloadSize)
}

// 默认设置
func (c *Config) defaultSetting() {
	c.Callback = &DefCallback{}
	c.maxDelayWriteNum = 10
	c.windowsMultipleTimesPayloadSize = 1.0
	c.delayWriteInitBufferSize = 8 * 1024
	c.maxDelayWriteDuration = 10 * time.Millisecond
	c.runInGoStrategy = taskStrategyBind
	c.tcpNoDelay = true
	// 对于text消息，默认不检查text是utf8字符
	c.utf8Check = func(b []byte) bool { return true }
}
