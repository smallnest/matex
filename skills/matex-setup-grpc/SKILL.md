---
name: Matex Setup gRPC
description: 在 matex 中接入 gRPC 微服务：配置 grpc 段、注册 gRPC 服务、初始化 gRPC client。当用户说"接 grpc / 配 grpc / setup grpc / gRPC 服务 / gRPC client / 微服务 / proto"时使用。
---

# 接入 gRPC 微服务

matex 的 gRPC 由 verticle 统一管理生命周期：配置 `grpc:` 段即创建 gRPC server，服务实现 `verticle.GRPCRegistrar` 接口注册，client 用 `env.GRPCClient` 初始化（自动 fail-fast 拨号 + 关闭清理）。

## 步骤

### 1. 配置（configs/config.yaml）

```yaml
grpc:
  addr: ":9090"        # gRPC 监听地址；留空/删除即禁用 server
  dial_timeout: 3s     # client 拨号超时
```

### 2. 定义 proto 并生成

把 `.proto` 放进 `proto/`（或 third_party），用 protoc 生成 `.pb.go`。生成的代码放 `gen/proto/`（不在 `internal/` 下，不受 depguard 约束）。

```sh
protoc --go_out=gen/proto --go-grpc_out=gen/proto -I proto proto/greeter.proto
```

### 3. 注册 gRPC 服务（internal/app/service.go）

让服务实现 `verticle.GRPCRegistrar`：

```go
func (s *DemoService) RegisterGRPC(g *grpc.Server) {
	pb.RegisterGreeterServer(g, s.greeter) // s.greeter 在 Setup 里构建
}
```

### 4. 初始化 gRPC client（Setup 里）

```go
func (s *DemoService) Setup(ctx context.Context, env *verticle.Env) error {
	conn, err := env.GRPCClient(ctx, "user-service:9090") // K8s Service DNS 名或 host:port
	if err != nil {
		return err
	}
	s.userClient = userpb.NewUserServiceClient(conn)
	// conn 由 verticle 在关闭时 Close，无需手动管理
	return nil
}
```

## 约定

- gRPC server 与 HTTP server 是**同一个进程的两个 listener**，由 verticle 一起启动、一起优雅退出（GracefulStop，超时强停）。
- 服务内通信默认**明文**（insecure，集群内网）。要 TLS 时给 `env.GRPCClient` 传 `grpc.WithTransportCredentials(...)`。
- `RegisterGRPC` 只在 `grpc.addr` 非空时被调用；未实现该接口则打 warning。
- client 连接在 `Setup` 里初始化（fail-fast：`grpc.dial_timeout` 内连不上就启动失败）。

## 验证

```sh
make run-min                                  # 无 grpc 段，纯 HTTP
make run                                      # 配置了 grpc 段，同时起 8080 + 9090
grpcurl -plaintext localhost:9090 list        # reflection 已注册可列出服务
```
