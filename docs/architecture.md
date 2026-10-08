# fio 架构

fio 是一个多协议网络库，底层是一套 epoll/kqueue 事件引擎，上面挂各协议的
状态机。这份文档定的是分层和各层的边界。

## 分层

```
github.com/antlabs/fio
├── engine/         事件循环 + 非阻塞连接 io + Handler 接口 + Listener
├── websocket/      WebSocket (rfc6455 + rfc7692) + 自己那套引擎
├── http/           HTTP/1.1
├── http2/          HTTP/2（RFC 9113 帧层 + 流 + HPACK）
├── http3/          HTTP/3（quic-go 做传输）
├── grpc/           gRPC（跑在 http2 上）
├── tls/            TLS 1.3 状态机（可以包在别的协议外面）
└── autobahn/       WebSocket 的 RFC 合规测试
```

## engine

职责：

| | |
|---|---|
| 事件循环 | epoll/kqueue（`MultiEventLoop` / `EventLoop`），ET 触发，fd 分片到循环上 |
| 连接 io | 非阻塞 fd 的读写、部分写、积压缓冲（`wbufList`）、读缓冲区自适应增长 |
| accept | `Listener`：非阻塞 accept + 停止标志（见下） |
| 池化 | 连接对象池、读缓冲区池（复用 wsutil/bytespool） |

不碰的东西：不认识任何协议的字节流。帧头、状态码、握手全部在协议包里。

### Handler 接口

协议包实现这个接口，引擎按事件调它：

```go
// engine
type Handler interface {
    // OnOpen 连接就绪。**在事件循环的 goroutine 上跑**，协议在这里
    // 初始化自己的状态。
    OnOpen(c *Conn)

    // OnData 有数据可读。返回"消化了多少字节"——引擎把这么多从读
    // 缓冲区里丢掉，剩下的留着下次和新的拼一起再喂。
    //
    // 返回 0 表示"还不够凑一条报文"，不是错误。
    OnData(c *Conn, buf []byte) (int, error)

    // OnClose 连接关闭，只会调一次。
    OnClose(c *Conn, err error)
}
```

**缓冲只有一处**：就是引擎的读缓冲区。协议实现不能自己再攒一份——两边都
攒的话同一段字节会被处理两次（`http/` 和 `tls/` 都在这上面栽过）。

引擎负责：把 fd 上的数据读进连接的读缓冲区、处理 EAGAIN/部分写、把写不出去
的部分攒起来、可写时补写。协议负责：这些字节是什么意思。

### OnOpen 的时机

`Add` 是 accept 循环（调用方的 goroutine）调的，而 `OnOpen` 必须在事件循环
的 goroutine 上跑——协议要初始化自己的状态（建状态机、`SetUserData`），那些
状态之后只被事件循环碰，两边一写一读就是数据竞争。

投递和 epoll 事件之间**没有先后保证**（`Add` 返回时数据可能已经到了、事件
已经排进 epoll），所以还有一条：事件处理那边看到 `activated` 位没置，就
**就地**把 `OnOpen` 补上（`activate` 是幂等的）。不这么做的话会先跑 `OnData`、
再跑 `OnOpen`，还是同一个竞争。

### Listener 为什么不用 net.Listener

`net.Listener` 的 `Accept` 是阻塞的，靠"关掉监听 fd"唤醒它。**这在 Linux 上
不成立**：`close()` 不会唤醒另一个线程里已经阻塞在 `accept()` 的调用——那个
线程持有文件描述的引用，fd 表项被删了它照样睡着。于是 `Close()` 永远等不到
accept 协程退出（实测：测试挂到超时；darwin 的 kqueue 会唤醒，所以本机跑得
通）。这种平台差异只有跨平台跑才打得出来。

`engine.Listener` 用非阻塞 accept + 停止标志：`Close()` 一置标志，最多 1ms
后循环自己退出，两个平台行为一致。

## 协议包

每个协议包导出自己的适配器，实现 `engine.Handler`，形状都是"把字节喂给状态
机，把状态机吐出来的字节写回连接"：

| 包 | 适配器 | 说明 |
|---|---|---|
| `http/` | `ConnHandler` | 请求解析、keep-alive、chunked |
| `http2/` | `ConnHandler` | 解帧、管流、HPACK；下游实现 `StreamHandler` |
| `grpc/` | `ServerHandler` | 实现 `http2.StreamHandler`：认 `:path`、切消息、发 trailer |
| `tls/` | `ConnHandler` | **包装器**：包在别的协议外面，见下 |
| `http3/` | — | 不走 epoll（见下） |

协议包之间可以有依赖：`grpc` 依赖 `http2`，`tls` 谁都能包。反过来不行
——`http2` 不该 import `grpc`。

### 叠起来

TLS 和别的协议不一样：**它包在别人外面**。所以它的适配器是个包装器——
`engine.OnData` 收到密文，解密后交给内层协议的 handler；内层要发的东西
经写拦截器（`Conn.SetWriteHook`）加密再出去。

```
engine（epoll / kqueue）
  ↕ 密文
tls.ConnHandler          握手、记录层、AEAD
  ↕ 明文
http2.ConnHandler        帧、流、HPACK
  ↕ 流事件
grpc.ServerHandler       :path、消息分帧、trailer
  ↕
业务
```

三层共用一个 fd、一块读缓冲区，全在同一个事件循环的 goroutine 上。
`grpc/stacked_test.go` 里有一条测试端到端跑这条路径，客户端用标准库的
`crypto/tls` + 官方 `x/net/http2.Framer`。

## 关于 HTTP/3 和 TLS

**HTTP/3** 是唯一一个不走 epoll 读事件的：它跑在 QUIC 上，UDP 收包后自己做
拥塞控制和流多路复用，没有内核的 TCP 连接可管。变长整数、包、帧这些是自己
实现的，传输用 quic-go。

**TLS** 用状态机实现，不调 `crypto/tls.Conn`：标准库那个 `Conn` 的握手**不是
分步的**——内部的 `handshakeErr` 一旦置上，后面每次 `Handshake()` 都直接返回
那个错误。所以在非阻塞 fd 上，第一次"数据不够"就把它废了（实测：喂完 1746
字节的 ServerHello 再调 Handshake，它一个字节都不读）。

状态机版本是"给一段密文、吐一段明文，不够就返回 need-more"，这样 TLS 连接
能和普通连接一样跑在同一个 epoll 上。

**应用密钥的派生时机是要害**（RFC 8446 7.1）：

```
client/server_application_traffic_secret_0 =
    Derive-Secret(Master Secret, "c ap traffic" / "s ap traffic",
                  CH..server Finished)
```

服务端在发完自己 Finished 时算好缓存起来；客户端在收到服务端 Finished、把它
写进 transcript 之后、发自己 Finished 之前算。**两边看到的 transcript 都是
CH..server Finished**。等收完对端 Finished 再算就多了一条消息、算出来是另一个
值——自己和自己测能过（两边错得一样），接标准库就报 record authentication
failed。

## 目录布局

```
fio/
├── engine/
│   ├── eventloop.go            事件循环
│   ├── multi_event_loops.go    多循环 + Add + activate
│   ├── conn.go                 连接（io + 缓冲）
│   ├── listener.go             accept 循环
│   ├── handler.go              Handler 接口
│   ├── syscall_*.go            recv/send（按平台分文件）
├── websocket/                  自带引擎（还没搬过来，见下）
├── http/  http2/  http3/  grpc/  tls/
├── autobahn/
└── docs/
```

## 已完成 / 接下去

已落地（每个都有测试，`-race` 全绿，Linux 和 macOS 都跑通）：

| 包 | 内容 |
|---|---|
| `engine/` | 事件循环 + 连接 io + `Handler` + `Listener` |
| `http/` | HTTP/1.1：keep-alive、pipelining、chunked 响应，接在 engine 上 |
| `http2/` | RFC 9113 帧层 + 流 + HPACK，接在 engine 上 |
| `grpc/` | 消息分帧 + trailer 状态，跑在 http2 上 |
| `tls/` | TLS 1.3 状态机，接在 engine 上，能包 HTTP/1.1 和 HTTP/2 |
| `http3/` | 端到端可用：QUIC 用 quic-go，变长整数/包/帧自己实现 |

接下去：

- **websocket/ 搬到 engine 上**：它的引擎和协议还焊在一起（`websocket/`
  目前不 import `engine/`）。搬的时候要保住两条路径——零拷贝（payload 直接
  指向读缓冲区）和攒包（cork）。不急着做，那条路径是压测第一名。
- **http2 的流控**：现在收发都不做窗口管理，大流量下对端可能因为窗口不动
  而卡住。要补 WINDOW_UPDATE 的收发。
- **QUIC 自己实现**：现在用 quic-go 做传输。要自己写的话是几千行的量级：
  TLS 1.3 握手集成、丢包重传、拥塞控制、两套流控窗口、连接迁移、0-RTT。
- **accept 分散到多个事件循环**：现在 `Listener` 是单个 goroutine 在 accept，
  高连接速率下可能成为瓶颈。要挂到 epoll 上（`SO_REUSEPORT` 或者把 lfd
  注册进循环）。
