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

import (
	"log/slog"
	"os"
	"runtime"
	"sync/atomic"
)

type MultiEventLoop struct {
	numLoops    int // 事件循环数量
	maxEventNum int
	loops       []*EventLoop
	globalTask  task
	runInIo     taskIo
	flag        evFlag // 是否使用io_uring
	level       slog.Level
	stat        // 统计信息
	*slog.Logger
}

var (
	defMaxEventNum   = 256
	defTaskMin       = 50
	defTaskMax       = 30000
	defTaskInitCount = 1000
	defNumLoops      = runtime.NumCPU() / 4
)

func (m *MultiEventLoop) initDefaultSetting() {
	m.level = slog.LevelError // 默认打印error级别的日志
	if m.numLoops == 0 {
		m.numLoops = max(defNumLoops, 1)
	}

	if m.maxEventNum == 0 {
		m.maxEventNum = defMaxEventNum
	}

	if m.globalTask.min == 0 {
		m.globalTask.min = max(defTaskMin/(m.numLoops+1), 1)
	} else {
		m.globalTask.min = max(m.globalTask.min/(m.numLoops+1), 1)
	}

	if m.globalTask.max == 0 {
		m.globalTask.max = max(defTaskMax/(m.numLoops+1), 1)
	} else {
		m.globalTask.max = max(m.globalTask.max/(m.numLoops+1), 1)
	}

	if m.globalTask.initCount == 0 {
		m.globalTask.initCount = max(defTaskInitCount/(m.numLoops+1), 1)
	} else {
		m.globalTask.initCount = max(m.globalTask.initCount/(m.numLoops+1), 1)
	}

	if m.flag == 0 {
		m.flag = EVENT_EPOLL
	}
}

func NewMultiEventLoopMust(opts ...EvOption) *MultiEventLoop {
	m, err := NewMultiEventLoop(opts...)
	if err != nil {
		panic(err)
	}

	m.Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: m.level}))
	return m
}

// 创建一个多路事件循环
func NewMultiEventLoop(opts ...EvOption) (e *MultiEventLoop, err error) {
	m := &MultiEventLoop{}

	m.initDefaultSetting()
	for _, o := range opts {
		o(m)
	}
	m.initDefaultSetting()
	m.globalTask.init()

	m.loops = make([]*EventLoop, m.numLoops)

	for i := 0; i < m.numLoops; i++ {
		m.loops[i], err = CreateEventLoop(m.maxEventNum, m.flag, m)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

// 启动多路事件循环
func (m *MultiEventLoop) Start() {
	for _, loop := range m.loops {
		go loop.Loop()
	}
}

func (m *MultiEventLoop) getEventLoop(fd int) *EventLoop {
	return m.loops[fd%len(m.loops)]
}

// 添加一个连接到多路事件循环
func (m *MultiEventLoop) add(c *Conn) error {
	fd := c.getFd()
	index := fd % len(m.loops)
	m.loops[index].conns.Store(fd, c)
	if err := m.loops[index].addRead(c); err != nil {
		m.loops[index].del(c)
		return err
	}
	atomic.AddInt64(&m.curConn, 1)
	return nil
}
