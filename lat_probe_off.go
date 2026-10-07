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

//go:build !greatws_latprobe

package greatws

// 不开探针时这几个是空操作, 编译器会整个消掉。
func probeMark() int64         { return 0 }
func probeObserve(start int64) {}

// GetLatProbe 是不开探针时的空实现, 免得 harness 里那个诊断路由编不过。
func (m *MultiEventLoop) GetLatProbe() ([]int64, int64) { return nil, 0 }
