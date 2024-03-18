// Copyright 2021-2024 antlabs. All rights reserved.
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
package window

import "sync/atomic"

// 滑动窗口记录历史go程数
type Window struct {
	// 历史go程数
	historyGo []int64
	sum       int64
	w         int64
}

func (w *Window) Init() {
	w.historyGo = make([]int64, 10)
}

func (w *Window) Add(goNum int64) {
	if len(w.historyGo) == 0 {
		return
	}

	needAdd := goNum - w.historyGo[w.w]

	atomic.AddInt64(&w.sum, needAdd)
	w.historyGo[w.w] = goNum
	w.w = (w.w + 1) % int64(len(w.historyGo))
}

func (w *Window) Avg() float64 {
	return float64(atomic.LoadInt64(&w.sum)) / float64(len(w.historyGo))
}
