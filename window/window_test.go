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

import (
	"slices"
	"testing"
)

func Test_Windows(t *testing.T) {
	t.Run("test_windows.0", func(t *testing.T) {
		var w Window
		w.Init()
		w.Add(1)
		if !slices.Equal(w.historyGo[:3], []int64{1, 0, 0}) {
			// 报错
			t.Errorf("w.historyGo is fail")
		}

		if w.Avg() > 0.5 {
			t.Errorf("w.avg > 0.5")
		}
	})
}
