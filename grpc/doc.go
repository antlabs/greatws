// Package grpc 是 quicknet 上的 gRPC 实现。
//
// gRPC 跑在 HTTP/2 上, 所以这个包依赖 http2: 它提供"HTTP/2 帧 + 流"这层,
// 本包负责 gRPC 自己的部分——长度前缀的消息分帧、状态码和 trailer、以及
// 由 proto 生成的代码要的那个编解码接口。
//
// 规划中的内容:
//
//   - 消息分帧: 1 字节压缩标志 + 4 字节大端长度, 和 HTTP/2 DATA 帧对齐
//   - 状态: 把 grpc-status / grpc-message 从 trailer 里解析出来
//   - 流: 一元调用和流式调用都映射到 HTTP/2 的流上
//
// 还没开始。
package grpc
