# observability — 日志与指标（obs）

`pkg/core/obs` = **slog JSON 日志** + **Prometheus 指标**。

## 运行

```sh
go run ./examples/observability

# trace id：不传就自动生成，传了就透传（响应头也会回写）
curl -i -H 'X-Request-ID: demo-1' localhost:8080/api/v1/work/alice

# 看日志（stdout）里的 service / trace_id / 字段
go run ./examples/observability 2>&1 | grep '"trace_id"'

# 看指标
curl -s localhost:8080/metrics | grep -E '^(observe_|http_server_)'

go test ./examples/observability
```

## 日志

启动时 `verticle.Run` 会调用 `obs.Init(name, level)`：JSON 输出到 stdout，每条都带
`"service"` 字段。**日志一律用 ctx 版本**，这样能自动带上 trace id：

```go
obs.Info(ctx, "user created", "user_id", 123, "dur_ms", 12)
// {"time":"…","level":"INFO","msg":"user created","service":"demo","trace_id":"a1b2c3d4","user_id":123,"dur_ms":12}
```

不要 `fmt.Println` 打日志，也不要自己 new logger —— `obs.Log(ctx)` 已经够用。

## trace id 从哪来

`httpx` 的 wrapper 在请求入口取 `X-Request-ID`，没有就生成一个 8 字节 hex：
写回响应头 → 塞进 `ctx` → 之后所有 `obs.*(ctx, …)` 和 dao 的慢查询日志都带它。

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
