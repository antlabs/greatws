package greatws

import (
	"slices"
	"testing"
)

func Test_Windows(t *testing.T) {
	t.Run("test_windows.0", func(t *testing.T) {
		var w windows
		w.init()
		w.add(1)
		if !slices.Equal(w.historyGo, []int64{1, 0, 0}) {
			// 报错
			t.Errorf("w.historyGo is fail")
		}

		if w.avg() > 0.5 {
			t.Errorf("w.avg > 0.5")
		}
	})
}
