---
name: Matex Setup rpcx
description: 在 matex 中接入 rpcx 微服务：配置 rpcx 段、注册 rpcx 服务、初始化 rpcx client。当用户说"接 rpcx / 配 rpcx / setup rpcx / rpcx 服务 / rpcx client / PRC 框架"时使用。
---

# 接入 rpcx 微服务

rpcx（github.com/smallnest/rpcx）是纯 Go 的 RPC 框架，**无需 IDL**——服务是普通 struct，方法签名 `func(ctx, *Args, *Reply) error`。matex 的 verticle 统一管理其生命周期：配置 `rpcx:` 段即创建 server，服务实现 `verticle.RPCXRegistrar` 注册，client 用 `env.RPCXClient` 初始化（自动关闭清理）。

## 步骤

### 1. 配置（configs/config.yaml）

```yaml
rpcx:
  network: tcp      # tcp | quic | kcp | ws | http
  addr: ":8972"     # 留空/删除即禁用
```

### 2. 定义服务（internal/domain/<name>/rpcx.go）

```go
type GreetArgs struct{ Name string }
type GreetReply struct{ Greeting string }

type RPCServer struct{ svc *Service }

func NewRPCServer(s *Service) *RPCServer { return &RPCServer{svc: s} }

// rpcx 方法签名：func(ctx, *Args, *Reply) error
func (r *RPCServer) Greet(ctx context.Context, args *GreetArgs, reply *GreetReply) error {
	resp, err := r.svc.Greet(ctx, args.Name)
	if err != nil { return err }
	reply.Greeting = resp.Greeting
	return nil
}
```

默认 gob 编码（Args/Reply 导出字段即可）；需要跨语言时改 msgpack/protobuf codec。

### 3. 注册（internal/app/service.go）

```go
func (s *DemoService) RegisterRPCX(g *rpcxserver.Server) error {
	return g.RegisterName("Greeter", helloworld.NewRPCServer(s.hello), "")
}
```

### 4. 初始化 client（Setup 里）

```go
// 点对点（直连一个地址）
xc, err := env.RPCXClient("Greeter", "demo:8972")   // 或 "tcp@demo:8972"
if err != nil { return err }

// 带注册中心（etcd 等，需 import rpcx client 包）
d, err := rpcxclient.NewEtcdV3Discovery(basePath, servicePath, []string{etcdAddr}, nil)
if err != nil { return err }
xc := env.RPCXClientWith("Greeter", d)

// 调用
reply := &GreetReply{}
err = xc.Call(ctx, "Greet", &GreetArgs{Name: "world"}, reply)
```

## 约定

- rpcx server 与 HTTP/gRPC 是**同一进程的 listener**，verticle 一起启动、一起优雅退出（`Shutdown(ctx)` 排空在途请求）。
- `RPCXClient` 默认 Failtry + RandomSelect + DefaultOption；需要 Failover/自定义重试时用 `RPCXClientWith` 自建。
- client 由 verticle 在关闭时 `Close`，无需手动管理。
- 服务发现：单实例直连用 `RPCXClient`；多实例/动态寻址用 `RPCXClientWith` + etcd/consul discovery（rpcx 原生支持）。

## gRPC vs rpcx 怎么选

| | gRPC | rpcx |
|---|---|---|
| IDL | 需要 proto + 代码生成 | 不需要（struct 即契约） |
| 跨语言 | 强（多语言生成） | Go 优先（msgpack/protobuf codec 可跨语言） |
| 传输 | HTTP/2 | tcp/quic/kcp/ws/http 可选 |
| 生态 | 业界标准，工具链丰富 | Go 生态内一体化（服务发现/熔断/限流插件） |

## 验证

```sh
make run                          # 同时起 8080(http) + 9090(grpc) + 8972(rpcx)
# rpcx 自带工具或写个一次性 client 调 Greeter.Greet 验证
```
