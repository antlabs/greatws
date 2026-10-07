// Package http 是 quicknet 上的 HTTP/1.1 实现。
//
// 状态和 websocket 包一样: 事件循环、连接 io、缓冲区、任务池都复用
// 同一个引擎, 它只负责"这些字节是一个 HTTP 请求/响应"。
//
// 规划中的内容:
//
//   - 请求解析: 请求行、header、chunked body, 状态机推进, 不阻塞事件循环
//   - 响应写出: 复用引擎的部分写和积攒(写不出去的部分交给可写事件)
//   - keep-alive: 一个连接上顺序处理多个请求
//   - 升级: 把连接交给 websocket 包(101 Switching Protocols)
//
// 还没开始。
package http
