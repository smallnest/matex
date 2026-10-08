# grpc — gRPC 服务与客户端

`pkg/core/grpcx` 管 gRPC 的**生命周期**：服务端由 `verticle.Run` 按 `grpc:` 段创建并
优雅关停，客户端在 `Setup` 里用 `env.GRPCClient` 拨号并自动注册关闭。

## 运行

```sh
go run ./examples/grpc
# http :8080 + grpc :9090

grpcurl -plaintext localhost:9090 grpc.health.v1.Health/Check     # 开了 reflection，免 proto
curl -i localhost:8080/api/v1/health                              # 未配 target → 503

go test ./examples/grpc        # 进程内起 server + dial，离线可跑
```

## 服务端

```go
// 1) 实现 verticle.GRPCRegistrar：注册生成的 service
func (s *Service) RegisterGRPC(g *grpc.Server) {
    pb.RegisterGreeterServer(g, s)
    reflection.Register(g)        // 可选：让 grpcurl 免 proto
}

// 2) 实现 grpc 方法（生成代码的接口）
func (s *Service) Say(ctx context.Context, req *pb.SayRequest) (*pb.SayReply, error) { … }
```

`grpc:` 段配了地址才会创建 server；没配就不起 gRPC，HTTP 照常工作。

## 客户端

```go
func (s *Service) Setup(ctx context.Context, env *verticle.Env) error {
    conn, err := env.GRPCClient(ctx, "user-svc:9090")   // host:port 或 K8s Service DNS
    if err != nil {
        return err        // fail-fast：启动就暴露依赖不可达
    }
    s.users = pb.NewUserClient(conn)
    return nil
}
```

- **fail-fast**：`env.GRPCClient` 会在 `grpc.dial_timeout`（默认 3s）内确认连接 READY，
  连不上就返回错误，服务启动失败。别把"依赖没起来"拖到第一个请求。
- **连接由框架关**：`env.GRPCClient` 内部 `AddCloser(conn.Close)`，你不需要手动关。
- **明文传输**：集群内默认 insecure（靠 VPC / mTLS-less mesh）。要对端 TLS 就自己
  传 `grpc.WithTransportCredentials(...)`，`env.GRPCClient` 支持可选 option。

## 没有注册中心

不做服务发现：地址由配置注入（K8s 的 Service DNS / 环境变量）。gRPC 自带的
客户端负载均衡（`grpc.WithDefaultServiceConfig` + headless Service）在需要时再叠加。

## 注意

- `Setup` 阶段**自己的 gRPC server 还没 serve**，所以 `target` 不能填自己，
  否则 dial 会超时导致启动失败。
- 生产建议给每个 handler 设超时：`ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)`。
