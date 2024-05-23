package main

import (
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/antlabs/greatws"
)

// https://github.com/snapview/tokio-tungstenite/blob/master/examples/autobahn-client.rs

const (
	// host = "ws://192.168.128.44:9003"
	host  = "ws://127.0.0.1:9005"
	agent = "greatws"
)

type handler struct {
	m *greatws.MultiEventLoop
}

type echoHandler struct {
	wg   *sync.WaitGroup
	done chan struct{}
}

func (e *echoHandler) OnOpen(c *greatws.Conn) {
	fmt.Printf("OnOpen::%p\n", c)
}

func (e *echoHandler) OnMessage(c *greatws.Conn, op greatws.Opcode, msg []byte) {
	// fmt.Printf("OnMessage: opcode:%s, msg.size:%d\n", op, len(msg))
	if op == greatws.Text || op == greatws.Binary {
		// os.WriteFile("./debug.dat", msg, 0o644)
		// if err := c.WriteMessage(op, msg); err != nil {
		// 	fmt.Println("write fail:", err)
		// }
		if err := c.WriteTimeout(op, msg, 1*time.Minute); err != nil {
			fmt.Println("write fail:", err)
		}
	}
}

func (e *echoHandler) OnClose(c *greatws.Conn, err error) {
	fmt.Println("OnClose:", c, err)
	// defer e.wg.Done()
	close(e.done)
}

func (h *handler) getCaseCount() int {
	var count int
	done := make(chan bool, 1)
	c, err := greatws.Dial(fmt.Sprintf("%s/getCaseCount", host), greatws.WithClientMultiEventLoop(h.m), greatws.WithClientOnMessageFunc(func() greatws.OnMessageFunc {
		return func(c *greatws.Conn, op greatws.Opcode, msg []byte) {
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
	c, err := greatws.Dial(fmt.Sprintf("%s/runCase?case=%d&agent=%s", host, caseNo, agent),
		greatws.WithClientReplyPing(),
		greatws.WithClientEnableUTF8Check(),
		greatws.WithClientDecompressAndCompress(),
		greatws.WithClientContextTakeover(),
		greatws.WithClientMaxWindowsBits(10),
		greatws.WithClientCallback(&echoHandler{done: done, wg: wg}),
		greatws.WithClientMultiEventLoop(h.m),
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
	c, err := greatws.Dial(fmt.Sprintf("%s/updateReports?agent=%s", host, agent), greatws.WithClientMultiEventLoop(h.m))
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
	h.m = greatws.NewMultiEventLoopMust(
		greatws.WithEventLoops(runtime.NumCPU()/2),
		greatws.WithBusinessGoNum(50, 10, 10000),
		greatws.WithMaxEventNum(1000),
		greatws.WithDisableParseInParseLoop(),
		greatws.WithLogLevel(slog.LevelError)) // epoll, kqueue

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
