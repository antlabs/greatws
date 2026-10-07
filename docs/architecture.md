# fio 架构

fio 是一个多协议网络库，底层是一套 epoll/kqueue 事件引擎，上面挂各协议的
状态机。这份文档定的是分层和各层的边界。

## 分层

```
github.com/antlabs/fio
├── websocket/      WebSocket (rfc6455 + rfc7692) + 引擎
├── http/           HTTP/1.1
├── grpc/           gRPC
└── (后面) http2/ http3/ tls/
```

现状：所有代码都在 `websocket/` 里——包括事件循环、缓冲区、任务池这些和协议
无关的东西。这是改名时"简单粗暴"搬过来的结果：先让结构立起来，再谈拆。

往后加协议（HTTP、gRPC）时，它们复用 `websocket/` 里的引擎部分。等第二个协议
真的落地、能看出哪些是公共的了，再把引擎从 `websocket/` 抽成 `engine/`——那时候
拆分有真实依据，不是拍脑袋。

## engine

职责：

| | |
|---|---|
| 事件循环 | epoll/kqueue（`MultiEventLoop` / `EventLoop`），accept、读写事件分发 |
| 连接 io | 非阻塞 fd 的读写、部分写、积压缓冲（`wbufList`） |
| 缓冲区 | 读缓冲区按需增长、攒包（cork）、池化（复用 wsutil/bytespool） |
| 任务池 | 回调的执行位置：就地（io 模式）或投递到分片 goroutine |
| 定时器 | 读写超时 |
| 统计 | 系统调用次数、重分配次数、移动字节数 |

不碰的东西：不认识任何协议的字节流。帧头、状态码、握手全部在协议包里。

### Handler 接口

协议包实现这个接口，引擎按事件调它：

```go
// engine
type Handler interface {
    // OnData 有数据可读。实现方从 c.Read() 拿字节，自己推进解析；
    // 返回 error 会让引擎关掉连接。
    OnData(c *Conn) error

    // OnOpen 连接就绪（WebSocket 是握手完成，HTTP 是收到第一个请求头）。
    OnOpen(c *Conn)

    // OnClose 连接关闭，只会调一次。
    OnClose(c *Conn, err error)
}
```

引擎负责：把 fd 上的数据读进连接的读缓冲区、处理 EAGAIN/部分写、把写不出去的
部分攒起来、可写时补写。协议负责：这些字节是什么意思。

## 协议包

每个协议包导出自己的 `Conn` 和 `Callback`，形状一样但方法不同：

| 包 | 连接类型 | 关键方法 |
|---|---|---|
| `websocket` | `websocket.Conn` | `WriteMessage(op, payload)`、`WritePing()` |
| `http1` | `http1.Conn` | `WriteResponse(resp)`、`ReadRequest()` |
| `http2` | `http2.Conn` | `WriteHeaders`、`WriteData`、流管理 |
| `http3` | `http3.Conn` | 同上，走 QUIC |
| `grpc` | `grpc.Conn` | 基于 http2 的 `WriteMessage`/`ReadMessage` |

协议包之间可以有依赖：`grpc` 依赖 `http2`，`http2`/`http1` 的 TLS 版本依赖
`tls`。反过来不行 —— `http2` 不该 import `grpc`。

## 关于 HTTP/3 和 TLS

**HTTP/3** 是唯一一个不走 epoll 读事件的：它跑在 QUIC 上，UDP 收包后自己做
拥塞控制和流多路复用，没有内核的 TCP 连接可管。它仍然用 `engine` 的缓冲区、
任务池和定时器，但事件源是自己的 UDP 收包循环，不是一个 epoll 注册的 fd。

**TLS** 用状态机实现，不调 `crypto/tls.Conn`：标准库那个 `Conn` 是阻塞语义
（一次 `Read` 要么给够要么等到够），套在非阻塞 fd 上会卡住整个事件循环。状态
机版本是"给一段密文、吐一段明文，不够就返回 need-more"，这样 TLS 连接能和
普通连接一样跑在同一个 epoll 上，也才能支撑海量连接。

## 目录布局

现在：

```
fio/
├── websocket/     全部代码（引擎 + 协议）
├── http/          HTTP/1.1（解析器已可用，连接还没接引擎）
├── grpc/          骨架，只有 doc.go
├── autobahn/      RFC 合规测试
└── docs/
```

拆出 `engine/` 之后：

```
fio/
├── engine/
│   ├── eventloop.go        事件循环
│   ├── conn.go             连接（io + 缓冲）
│   ├── syscall_linux.go    recvfrom/sendto/sendmsg
│   ├── syscall_other.go
│   ├── cork.go             攒包
│   ├── bufseg.go           小消息拼块
│   ├── taskpool.go         回调执行
│   └── options.go          引擎选项
├── websocket/
│   ├── conn.go             帧状态机、WriteMessage
│   ├── upgrade.go          net/http 升级
│   ├── client.go           拨号
│   ├── frame.go            帧头编码
│   ├── deflate.go          rfc7692
│   └── callback.go
├── http/  http2/  http3/  grpc/  tls/
└── docs/
```

## 已完成 / 接下去

已落地：`engine/`（事件循环 + 连接 io + Handler 接口）、`tls/`（状态机
TLS，用 goroutine 装 crypto/tls 的握手）、`http2/`（帧层 + 流 + HPACK）、
`grpc/`（消息分帧 + trailer 状态，端到端跑通）。

接下去：

- **websocket/ 搬到 engine 上**：它的引擎和协议还焊在一起。搬的时候要保住
  两条路径——零拷贝（payload 直接指向读缓冲区）和攒包（cork，一轮 read
  里多条 frame 的回包合成一次写）。不急着做，那条路径是压测第一名。
- **http/ 接引擎**：解析器（httparser）已经能用了，缺的是把它接到
  `engine.Handler` 上、以及响应写出。
- **http2 的流控和优先级**：现在收发都不做窗口管理，大流量下对端可能
  因为窗口不动而卡住。要补 WINDOW_UPDATE 的收发。
- **HTTP/3**：跑在 QUIC 上，是唯一不走 epoll 读事件的——UDP 收包后自己
  做拥塞控制和流多路复用。
