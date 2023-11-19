# bigws
支持海量连接的websocket库，callback写法

# 特性
* 支持 epoll/kqueue
* 低内存占用
* 高tps

# 暂不支持
* ssl
* windows
* io-uring

# 例子-服务端
```go

type echoHandler struct{}

func (e *echoHandler) OnOpen(c *bigws.Conn) {
	// fmt.Printf("OnOpen: %p\n", c)
}

func (e *echoHandler) OnMessage(c *bigws.Conn, op bigws.Opcode, msg []byte) {
	if err := c.WriteTimeout(op, msg, 3*time.Second); err != nil {
		fmt.Println("write fail:", err)
	}
	// if err := c.WriteMessage(op, msg); err != nil {
	// 	slog.Error("write fail:", err)
	// }
}

func (e *echoHandler) OnClose(c *bigws.Conn, err error) {
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	slog.Error("OnClose:", errMsg)
}

type handler struct {
	m *bigws.MultiEventLoop
}

func (h *handler) echo(w http.ResponseWriter, r *http.Request) {
	c, err := bigws.Upgrade(w, r,
		bigws.WithServerReplyPing(),
		// bigws.WithServerDecompression(),
		bigws.WithServerIgnorePong(),
		bigws.WithServerCallback(&echoHandler{}),
		// bigws.WithServerEnableUTF8Check(),
		bigws.WithServerReadTimeout(5*time.Second),
		bigws.WithServerMultiEventLoop(h.m),
	)
	if err != nil {
		slog.Error("Upgrade fail:", "err", err.Error())
	}
	_ = c
}

func main() {

	var h handler

	h.m = bigws.NewMultiEventLoopMust(bigws.WithEventLoops(0), bigws.WithMaxEventNum(1000), bigws.WithLogLevel(slog.LevelError)) // epoll, kqueue
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
