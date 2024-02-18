# greatws
支持海量连接的websocket库，callback写法

![Go](https://github.com/antlabs/greatws/workflows/Go/badge.svg)
[![codecov](https://codecov.io/gh/antlabs/greatws/branch/master/graph/badge.svg)](https://codecov.io/gh/antlabs/greatws)
[![Go Report Card](https://goreportcard.com/badge/github.com/antlabs/greatws)](https://goreportcard.com/report/github.com/antlabs/greatws)

## 处理流程
![greatws.png](https://github.com/antlabs/images/blob/main/greatws/greatws.png?raw=true)

# 特性
* 支持 epoll/kqueue
* 低内存占用
* 高tps

# 暂不支持
* ssl
* windows
* io-uring

# 警告⚠️
早期阶段，暂时不建议生产使用

# 例子-服务端
```go

type echoHandler struct{}

func (e *echoHandler) OnOpen(c *greatws.Conn) {
	// fmt.Printf("OnOpen: %p\n", c)
}

func (e *echoHandler) OnMessage(c *greatws.Conn, op greatws.Opcode, msg []byte) {
	if err := c.WriteTimeout(op, msg, 3*time.Second); err != nil {
		fmt.Println("write fail:", err)
	}
	// if err := c.WriteMessage(op, msg); err != nil {
	// 	slog.Error("write fail:", err)
	// }
}

func (e *echoHandler) OnClose(c *greatws.Conn, err error) {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	slog.Error("OnClose:", errMsg)
}

type handler struct {
	m *greatws.MultiEventLoop
}

func (h *handler) echo(w http.ResponseWriter, r *http.Request) {
	c, err := greatws.Upgrade(w, r,
		greatws.WithServerReplyPing(),
		// greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		// greatws.WithServerEnableUTF8Check(),
		greatws.WithServerReadTimeout(5*time.Second),
		greatws.WithServerMultiEventLoop(h.m),
	)
	if err != nil {
		slog.Error("Upgrade fail:", "err", err.Error())
	}
	_ = c
}

func main() {

	var h handler

	h.m = greatws.NewMultiEventLoopMust(greatws.WithEventLoops(0), greatws.WithMaxEventNum(256), greatws.WithLogLevel(slog.LevelError)) // epoll, kqueue
	h.m.Start()
	fmt.Printf("apiname:%s\n", h.m.GetApiName())

	mux := &http.ServeMux{}
	mux.HandleFunc("/autobahn", h.echo)

	rawTCP, err := net.Listen("tcp", ":9001")
	if err != nil {
		fmt.Println("Listen fail:", err)
		return
	}
}
```
## 100w websocket长链接测试
### e5 洋垃圾机器
* cpu=e5 2686(单路)
* memory=32GB
```
BenchType  : BenchEcho
Framework  : greatws
TPS        : 27954
EER        : 225.42
Min        : 35.05us
Avg        : 1.79s
Max        : 2.74s
TP50       : 1.88s
TP75       : 1.95s
TP90       : 1.99s
TP95       : 2.02s
TP99       : 2.09s
Used       : 178.86s
Total      : 5000000
Success    : 5000000
Failed     : 0
Conns      : 1000000
Concurrency: 50000
Payload    : 1024
CPU Min    : 41.62%
CPU Avg    : 124.01%
CPU Max    : 262.72%
MEM Min    : 555.25M
MEM Avg    : 562.44M
MEM Max    : 626.47M
```

### 5800h cpu
* cpu=5800h
* memory=64GB
```
BenchType  : BenchEcho
Framework  : greatws
TPS        : 82088
EER        : 447.72
Min        : -1ns
Avg        : 605.25ms
Max        : 1.68s
TP50       : 609.79ms
TP75       : 709.26ms
TP90       : 761.86ms
TP95       : 771.77ms
TP99       : 779.10ms
Used       : 50.47s
Total      : 5000000
Success    : 4142842
Failed     : 857158
Conns      : 1000000
Concurrency: 50000
Payload    : 1024
CPU Min    : 114.33%
CPU Avg    : 183.35%
CPU Max    : 280.22%
MEM Min    : 625.27M
MEM Avg    : 632.89M
MEM Max    : 666.96M
```