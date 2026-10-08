# lifecycle — 服务生命周期

一个部署单元 = 一个 `verticle.Service`：

```go
type Service interface {
    Name() string                                     // 配置段 key / 日志前缀 / metrics label
    Setup(ctx context.Context, env *verticle.Env) error
    BuildRouter(srv *httpx.Server) error
}
```

`verticle.Run(ctx, svc)` 统一驱动整条生命周期。

## 运行

```sh
go run ./examples/lifecycle

curl -i localhost:8080/readyz                        # 前 3s 是 503（ready_window 预热）
sleep 4; curl -i localhost:8080/readyz               # 之后 200
curl -i localhost:8080/api/v1/greeting/world

# 热更：改 examples/lifecycle/config.yaml 里的 greeting，保存
# 日志立刻出现 "config reloaded"，接口返回值跟着变，不用重启

# Ctrl-C：观察优雅退出的顺序
```

## 启动顺序（fail-fast，任一步失败即退出）

```
加载 config.yaml
  → obs.Init（日志先就绪）
  → 创建 metrics registry
  → 初始化配了的基础设施（db/redis/memcache/kafka，没配的为 nil）
  → svc.Setup(ctx, env)
  → httpx server + /healthz /readyz /metrics
  → svc.BuildRouter(srv)
  → 开始服务 + 监听配置变化
```

## 退出顺序（SIGINT/SIGTERM）

```
http drain(10s) → 等 blocks(10s) → closers（逆序）→ infra close
```

## 三个可选钩子

```go
// 1) 后台任务：随应用启动，ctx 取消时必须返回（返回非 nil error 会拖垮整个应用）
env.Block("heartbeat", func(ctx context.Context) error {
    t := time.NewTicker(time.Second); defer t.Stop()
    for {
        select {
        case <-ctx.Done(): return nil
        case <-t.C():      obs.Info(ctx, "tick")
        }
    }
})

// 2) 清理：逆序执行；infra 的 closer 在外层，所以"先关服务，再关 infra"
env.AddCloser(func() { obs.Info(context.Background(), "flushing") })

// 3) 就绪：叠加在默认就绪检查（ping 已配置的 infra）之上
func (s *Service) Ready(ctx context.Context) error { … }
```

## 配置热更

实现 `OnServiceConfigChange(raw map[string]any)` 即可（`verticle.ConfigWatcher`）：

```go
func (s *Service) OnServiceConfigChange(raw map[string]any) {
    var cfg Config
    if err := config.ParseMap(raw, &cfg); err != nil {
        obs.Warn(ctx, "reload rejected", "err", err)  // 解析失败就保持旧值
        return
    }
    s.mu.Lock(); s.cfg = cfg; s.mu.Unlock()           // 记得加锁：watcher 与 handler 并发
}
```

只有**服务专属段**会热更；框架段（http/grpc/db/…）改了需要重启——这是刻意的，
免得运行中换数据库这种事悄悄发生。热更的单位是 `Name()` 对应的那一段。
