package greatws

// 滑动窗口记录历史go程数
type windows struct {
	// 历史go程数
	historyGo []int64
	sum       int64
	w         int64
}

func newWindows(size int) *windows {
	return &windows{
		historyGo: make([]int64, size, size),
	}
}

func (w *windows) add(goNum int64) {
	if len(w.historyGo) == 0 {
		return
	}

	w.sum += goNum - w.historyGo[w.w]
	w.historyGo[w.w] = goNum
	w.w = (w.w + 1) % int64(len(w.historyGo))
}

func (w *windows) avg() float64 {
	return float64(w.sum) / float64(len(w.historyGo))
}
