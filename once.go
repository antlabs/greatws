package greatws

import (
	"sync"
	"sync/atomic"
)

type myOnce struct {
	done uint32
}

func (o *myOnce) Do(mu *sync.Mutex, f func()) {

	if atomic.LoadUint32(&o.done) == 0 {

		o.doSlow(mu, f)
	}
}

// 这里与标准库的做法不一样，标准库是保证了f执行之后才会设置done为1
// 这里是先设置done为1，然后再执行f
// 标准库是站在资源必须初始化成功的角度设计，而这里主要是为了解决资源释放自己调用自己的死锁问题
// 试想下这个场景;
// 出错会调用用户的OnClose函数，这个函数会被包在sync.Once里面，而OnClose函数中又会调用Close函数，这个函数里面也会有调用OnClose的逻辑，这样就会死锁
// 如果前置设置done为1，那么就不会有这个问题
func (o *myOnce) doSlow(mu *sync.Mutex, f func()) {
	mu.Lock()
	defer mu.Unlock()
	if o.done == 0 {
		atomic.StoreUint32(&o.done, 1)
		f()
	}
}
