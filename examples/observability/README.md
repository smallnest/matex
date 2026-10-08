# observability — 日志、指标与链路（obs）

`pkg/core/obs` = **slog JSON 日志** + **Prometheus 指标** + **OpenTelemetry 链路**。

## 运行

```sh
go run ./examples/observability

# 相关 id：不传就自动生成，传了就透传（响应头也会回写）
curl -i -H 'X-Request-ID: demo-1' localhost:8080/api/v1/work/alice

# 看日志（stdout）里的 service / trace_id / 字段
go run ./examples/observability 2>&1 | grep '"trace_id"'

# 看指标
curl -s localhost:8080/metrics | grep -E '^(observe_|http_server_)'

# 看链路（先把 config.yaml 的 trace.enabled 改成 true）
curl -i -H 'traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' \
  localhost:8080/api/v1/work/alice

go test ./examples/observability
```

## 日志

启动时 `verticle.Run` 会调用 `obs.Init(name, level)`：JSON 输出到 stdout，每条都带
`"service"` 字段。**日志一律用 ctx 版本**，这样能自动带上相关 id：

```go
obs.Info(ctx, "user created", "user_id", 123, "dur_ms", 12)
// {"time":"…","level":"INFO","msg":"user created","service":"demo","trace_id":"a1b2c3d4","user_id":123,"dur_ms":12}
```

不要 `fmt.Println` 打日志，也不要自己 new logger —— `obs.Log(ctx)` 已经够用。

## 相关 id 从哪来

按优先级取第一个可用的：

1. **开了 tracing** → 当前 span 的 trace id（32 位 hex，跨服务唯一，能直接去追踪后端查）
2. 入站的 `X-Request-ID`（客户端自己带的相关 id）
3. 都没有 → 生成一个 8 字节 hex

结果写回响应头 `X-Request-ID` 并塞进 `ctx`，之后所有 `obs.*(ctx, …)` 和 dao 慢查询日志
都带它。**没开 tracing 时行为和接入前完全一致**。

## 链路（OpenTelemetry）

默认关闭。开与不开的差别只有配置：

```yaml
trace:
  enabled: true
  endpoint: "localhost:4317"   # OTLP/gRPC collector；留空则只生成 span、不外发
  sample_ratio: 1.0            # 0~1，父级已采样的链路始终保留
```

开启后：

- `httpx` 给每个请求开一个 **server span**，span 名用路由模板
  （`GET /api/v1/work/{name}`，低基数），并自动续接入站的 `traceparent`；
- 业务代码用 `obs.StartSpan` 加子 span，**关着的时候是 no-op，不用写 `if tracing` 分支**：

```go
ctx, span := obs.StartSpan(ctx, "compute")
defer span.End()
```

- 调用下游时把链路带出去：

```go
h := http.Header{}
obs.InjectHTTP(ctx, h)   // 写入 traceparent，下游服务续接同一条链路
```

- 优雅退出时 `verticle` 会 flush 最后一批 span。

跨度命名只用**路由模板**而不是原始路径 —— 每个 URL 都建一个 span 名会把后端打爆。

## 指标

| 指标 | 类型 | 标签 |
|---|---|---|
| `http_server_requests_total` | counter | `route` `method` `code` |
| `http_server_request_duration_seconds` | histogram | `route` `method` `code` |
| `go_*` / `process_*` | runtime / 进程 | — |

**自定义指标在 `Setup` 里注册**（`env.Metrics` 在 `Setup` 之前就已就绪）：

```go
func (s *Service) Setup(ctx context.Context, env *verticle.Env) error {
	s.work = env.Metrics.Counter("observe_work_total", "processed calls", "name")
	s.value = env.Metrics.Gauge("observe_last_value", "last computed value")
	return nil
}

// handler 里
	s.work.WithLabelValues(name).Inc()
```

注册同一个名字两次会 panic —— 每个指标只在 `Setup` 注册一次。
