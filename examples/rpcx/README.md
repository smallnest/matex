# rpcx — 无需 IDL 的 RPC

`pkg/core/rpcx` 管 rpcx 的生命周期。rpcx 和 gRPC 最大的区别：**没有 IDL、没有代码生成**——
一个服务就是一个普通 struct，导出方法签名对上就能被调用：

```go
func (s *Service) Method(ctx context.Context, args *Args, reply *Reply) error
```

## 运行

```sh
go run ./examples/rpcx
# http :8080 + rpcx :8972

curl -i localhost:8080/api/v1/greet/world          # 本地调用
curl -i localhost:8080/api/v1/greet-remote/world   # 真实 rpcx 往返（默认 target 指向自己）

go test ./examples/rpcx        # 进程内起 server + 真 client 调用，离线可跑
```

## 服务端

```go
type Greeter struct{ prefix string }

func (g *Greeter) Greet(ctx context.Context, args *GreetArgs, reply *GreetReply) error {
    reply.Greeting = g.prefix + args.Name
    return nil
}

// 实现 verticle.RPCXRegistrar
func (s *Service) RegisterRPCX(srv *rpcxserver.Server) error {
    return srv.RegisterName("Greeter", &Greeter{}, "")   // 注册名 = 客户端的 servicePath
}
```

- 参数/返回值必须是**导出字段**的 struct（默认 gob 编码）。
- 注册名（`"Greeter"`）要和客户端 `PeerClient("Greeter", …)` 一致。
- 服务端在 `rpcx:` 段配了地址时才创建。

## 客户端

```go
c, err := env.RPCXClient("Greeter", "user-svc:8972")   // 或 "quic@user-svc:8972"
if err != nil { return err }
s.client = c

// 调用
var reply GreetReply
err = s.client.Call(ctx, "Greet", &GreetArgs{Name: "world"}, &reply)
```

- **懒连接**：和 gRPC 不同，`RPCXClient` 不在 Setup 时探测连通性，第一次 `Call` 才拨号
  （所以 target 填自己也能跑通）。想在启动时就校验依赖，得自己 `Call` 一个 ping 方法。
- 策略固定为 `Failtry + RandomSelect`（单点重试 + 随机选节点）。
- 需要 etcd/consul 注册中心、多服务发现时用 `env.RPCXClientWith(servicePath, discovery)`。

## 错误处理

RPC 边界上**只有错误消息能过去**，`errs.Kind` / 业务 code 不会——所以客户端要把错误
再映射回自己的 code：

```go
if err := s.client.Call(ctx, "Greet", args, reply); err != nil {
    return nil, errs.Unavailable(50302, "rpcx call failed: %v", err)
}
```

## gRPC 还是 rpcx

| | gRPC | rpcx |
|---|---|---|
| IDL / 代码生成 | 需要（protobuf） | 不需要 |
| 契约 | 强（`.proto`） | 弱（struct 即约定） |
| 多语言互通 | 好 | Go 为主 |
| 传输 | HTTP/2 | tcp/quic/kcp/ws/http |

跨语言、要强契约 → gRPC；纯 Go 内部服务、想少一层生成代码 → rpcx。
