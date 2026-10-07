package main

import (
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/antlabs/quicknet"
)

// https://github.com/snapview/tokio-tungstenite/blob/master/examples/autobahn-client.rs

const (
	// host = "ws://192.168.128.44:9003"
	host  = "ws://127.0.0.1:9005"
	agent = "quicknet"
)

type handler struct {
	m *quicknet.MultiEventLoop
}

type echoHandler struct {
	wg   *sync.WaitGroup
	done chan struct{}
}

func (e *echoHandler) OnOpen(c *quicknet.Conn) {
	fmt.Printf("OnOpen::%p\n", c)
}

func (e *echoHandler) OnMessage(c *quicknet.Conn, op quicknet.Opcode, msg []byte) {
	// fmt.Printf("OnMessage: opcode:%s, msg.size:%d\n", op, len(msg))
	if op == quicknet.Text || op == quicknet.Binary {
		// os.WriteFile("./debug.dat", msg, 0o644)
		// if err := c.WriteMessage(op, msg); err != nil {
		// 	fmt.Println("write fail:", err)
		// }
		if err := c.WriteTimeout(op, msg, 1*time.Minute); err != nil {
			fmt.Println("write fail:", err)
		}
	}
}

func (e *echoHandler) OnClose(c *quicknet.Conn, err error) {
	fmt.Println("OnClose:", c, err)
	// defer e.wg.Done()
	close(e.done)
}

func (h *handler) getCaseCount() int {
	var count int
	done := make(chan bool, 1)
	c, err := quicknet.Dial(fmt.Sprintf("%s/getCaseCount", host), quicknet.WithClientMultiEventLoop(h.m), quicknet.WithClientOnMessageFunc(func() quicknet.OnMessageFunc {
		return func(c *quicknet.Conn, op quicknet.Opcode, msg []byte) {
			var err error
			count, err = strconv.Atoi(string(msg))
			if err != nil {
				panic(err)
			}
			done <- true
			fmt.Printf("msg(%s)\n", msg)
			c.Close()
		}
	}()))
	if err != nil {
		panic(err)
	}
	defer c.Close()

	err = c.ReadLoop()
	<-done
	fmt.Printf("readloop rv:%s\n", err)
	return count
}

func (h *handler) runTest(caseNo int, wg *sync.WaitGroup) {
	done := make(chan struct{})
	c, err := quicknet.Dial(fmt.Sprintf("%s/runCase?case=%d&agent=%s", host, caseNo, agent),
		quicknet.WithClientReplyPing(),
		quicknet.WithClientEnableUTF8Check(),
		quicknet.WithClientDecompressAndCompress(),
		quicknet.WithClientContextTakeover(),
		quicknet.WithClientMaxWindowsBits(10),
		quicknet.WithClientCallback(&echoHandler{done: done, wg: wg}),
		quicknet.WithClientMultiEventLoop(h.m),
	)
	if err != nil {
		fmt.Println("Dial fail:", err)
		return
	}

	go func() {
		_ = c.ReadLoop()
	}()
	<-done
}

func (h *handler) updateReports() {
	c, err := quicknet.Dial(fmt.Sprintf("%s/updateReports?agent=%s", host, agent), quicknet.WithClientMultiEventLoop(h.m))
	if err != nil {
		fmt.Println("Dial fail:", err)
		return
	}

	c.Close()
}

// 1.先通过接口获取case的总个数
// 2.运行测试客户端client
func main() {
	var h handler
	h.m = quicknet.NewMultiEventLoopMust(
		quicknet.WithEventLoops(runtime.NumCPU()/2),
		quicknet.WithBusinessGoNum(50, 10, 10000),
		quicknet.WithMaxEventNum(1000),
		quicknet.WithLogLevel(slog.LevelError)) // epoll, kqueue

	h.m.Start()
	total := h.getCaseCount()
	var wg sync.WaitGroup
	// wg.Add(total)
	fmt.Println("total case:", total)
	for i := 1; i <= total; i++ {
		h.runTest(i, &wg)
	}
	// wg.Wait()
	h.updateReports()
}
