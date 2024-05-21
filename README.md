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
* 对websocket的兼容性较高，完整实现rfc6455, rfc7692

# 暂不支持

* ssl
* windows
* io-uring

# 警告⚠️

早期阶段，暂时不建议生产使用

## 内容
* [安装](#Installation)
* [例子](#example)
	* [net/http升级到websocket服务端](#net-http升级到websocket服务端)
	* [gin升级到websocket服务端](#gin升级到websocket服务端)
	* [客户端](#客户端)
* [配置函数](#配置函数)
	* [客户端配置参数](#客户端配置)
		* [配置header](#配置header)
		* [配置握手时的超时时间](#配置握手时的超时时间)
		* [配置自动回复ping消息](#配置自动回复ping消息)
		* [配置客户端最大读取message](#配置客户端最大读message)
	* [服务配置参数](#服务端配置)
		* [配置服务自动回复ping消息](#配置服务自动回复ping消息)
		* [配置服务端最大读取message](#配置服务端最大读message)
# 例子-服务端
### net http升级到websocket服务端
```go

package main

import (
	"fmt"

	"github.com/antlabs/greatws"
)

type echoHandler struct{}

func (e *echoHandler) OnOpen(c *greatws.Conn) {
 // fmt.Printf("OnOpen: %p\n", c)
}

func (e *echoHandler) OnMessage(c *greatws.Conn, op greatws.Opcode, msg []byte) {
 if err := c.WriteTimeout(op, msg, 3*time.Second); err != nil {
  fmt.Println("write fail:", err)
 }
 // if err := c.WriteMessage(op, msg); err != nil {
 //  slog.Error("write fail:", err)
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
 log.Println("non-tls server exit:", http.Serve(rawTCP, mux))
}
```
[返回](#内容)

### gin升级到websocket服务端
```go
package main

import (
	"fmt"

	"github.com/antlabs/greatws"
	"github.com/gin-gonic/gin"
)

type handler struct{
    m *greatws.MultiEventLoop
}

func (h *handler) OnOpen(c *greatws.Conn) {
	fmt.Printf("服务端收到一个新的连接")
}

func (h *handler) OnMessage(c *greatws.Conn, op greatws.Opcode, msg []byte) {
	// 如果msg的生命周期不是在OnMessage中结束，需要拷贝一份
	// newMsg := make([]byte, len(msg))
	// copy(newMsg, msg)

	fmt.Printf("收到客户端消息:%s\n", msg)
	c.WriteMessage(op, msg)
	// os.Stdout.Write(msg)
}

func (h *handler) OnClose(c *greatws.Conn, err error) {
	fmt.Printf("服务端连接关闭:%v\n", err)
}

func main() {
	r := gin.Default()
	var h handler
	h.m = greatws.NewMultiEventLoopMust(greatws.WithEventLoops(0), greatws.WithMaxEventNum(256), greatws.WithLogLevel(slog.LevelError)) // epoll, kqueue
	h.m.Start()

	r.GET("/", func(c *gin.Context) {
		con, err := greatws.Upgrade(c.Writer, c.Request, greatws.WithServerCallback(h.m), greatws.WithServerMultiEventLoop(h.m))
		if err != nil {
			return
		}
		con.StartReadLoop()
	})
	r.Run()
}
```
[返回](#内容)

### 客户端
```go
package main

import (
	"fmt"
	"time"

	"github.com/antlabs/greatws"
)

var m *greatws.MultiEventLoop
type handler struct{}

func (h *handler) OnOpen(c *greatws.Conn) {
	fmt.Printf("客户端连接成功\n")
}

func (h *handler) OnMessage(c *greatws.Conn, op greatws.Opcode, msg []byte) {
	// 如果msg的生命周期不是在OnMessage中结束，需要拷贝一份
	// newMsg := make([]byte, len(msg))
	// copy(newMsg, msg)

	fmt.Printf("收到服务端消息:%s\n", msg)
	c.WriteMessage(op, msg)
	time.Sleep(time.Second)
}

func (h *handler) OnClose(c *greatws.Conn, err error) {
	fmt.Printf("客户端端连接关闭:%v\n", err)
}

func main() {
	m = greatws.NewMultiEventLoopMust(greatws.WithEventLoops(0), greatws.WithMaxEventNum(256), greatws.WithLogLevel(slog.LevelError)) // epoll, kqueue
	m.Start()
	c, err := greatws.Dial("ws://127.0.0.1:8080/", greatws.WithClientCallback(&handler{}), greatws.WithServerMultiEventLoop(h.m))
	if err != nil {
		fmt.Printf("连接失败:%v\n", err)
		return
	}

	c.WriteMessage(opcode.Text, []byte("hello"))
	time.Sleep(time.Hour) //demo里面等待下OnMessage 看下执行效果，因为greatws.Dial和WriteMessage都是非阻塞的函数调用，不会卡住主go程
}
```
[返回](#内容)
## 配置函数
### 客户端配置参数
#### 配置header
```go
func main() {
	greatws.Dial("ws://127.0.0.1:12345/test", greatws.WithClientHTTPHeader(http.Header{
		"h1": "v1",
		"h2":"v2", 
	}))
}
```
[返回](#内容)
#### 配置握手时的超时时间
```go
func main() {
	greatws.Dial("ws://127.0.0.1:12345/test", greatws.WithClientDialTimeout(2 * time.Second))
}
```
[返回](#内容)

#### 配置自动回复ping消息
```go
func main() {
	greatws.Dial("ws://127.0.0.1:12345/test", greatws.WithClientReplyPing())
}
```
[返回](#内容)
#### 配置客户端最大读message
```go
	// 限制客户端最大服务返回返回的最大包是1024，如果超过这个大小报错
	greatws.Dial("ws://127.0.0.1:12345/test", greatws.WithClientReadMaxMessage(1024))
```
[返回](#内容)
### 服务端配置参数
#### 配置服务自动回复ping消息
```go
func main() {
	c, err := greatws.Upgrade(w, r, greatws.WithServerReplyPing())
        if err != nil {
                fmt.Println("Upgrade fail:", err)
                return
        }   
}
```
[返回](#内容)

#### 配置服务端最大读message
```go
func main() {
	// 配置服务端读取客户端最大的包是1024大小, 超过该值报错
	c, err := greatws.Upgrade(w, r, greatws.WithServerReadMaxMessage(1024))
        if err != nil {
                fmt.Println("Upgrade fail:", err)
                return
        }   
}
```
[返回](#内容)
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
TPS        : 103544
EER        : 397.07
Min        : 26.51us
Avg        : 95.79ms
Max        : 1.34s
TP50       : 58.26ms
TP75       : 60.94ms
TP90       : 62.50ms
TP95       : 63.04ms
TP99       : 63.47ms
Used       : 40.76s
Total      : 5000000
Success    : 4220634
Failed     : 779366
Conns      : 1000000
Concurrency: 10000
Payload    : 1024
CPU Min    : 30.54%
CPU Avg    : 260.77%
CPU Max    : 335.88%
MEM Min    : 432.25M
MEM Avg    : 439.71M
MEM Max    : 449.62M
```
