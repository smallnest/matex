---
name: Matex Setup Tracing
description: 在 matex 中接入分布式链路追踪（OpenTelemetry）：开启 trace 配置段、给业务代码加 span、跨服务传播 traceparent、与日志 trace_id 对齐。当用户说"接链路追踪 / 接 tracing / 接 otel / 开 trace / setup tracing / 分布式追踪 / 全链路 / traceparent / Jaeger / Tempo"时使用。
---

# 接入链路追踪（OpenTelemetry）

链路经 `pkg/core/obs` 使用（OTel SDK + OTLP exporter 薄封装），业务不 import OTel API，
也不写 `if tracing` 分支 —— 关闭时全部是 no-op。

## 步骤

### 1. 配置（configs/config.yaml）

```yaml
trace:
  enabled: true
  endpoint: "otel-collector.observability:4317"   # OTLP/gRPC；留空则只生成 span、不外发
  sample_ratio: 1.0        # 0~1；父级已采样的链路始终保留（ParentBased）
  export_timeout: 10s
```

留空/删除 `trace:` 段即关闭（默认关闭）。`verticle` 会在 `obs.Init` 之后调用
`obs.InitTracing`，并在优雅退出时 flush 最后一批 span。

### 2. 业务代码加子 span

```go
func (s *Service) Do(ctx context.Context, id int64) (*Thing, error) {
	ctx, span := obs.StartSpan(ctx, "thing.load")
	defer span.End()

	t, err := s.load(ctx, id)
	if err != nil { return nil, err }
	return t, nil
}
```

`obs.StartSpan` 返回 `(ctx, span)`：**必须用返回的 ctx**，否则子 span 挂不到链路上。

### 3. 调用下游时把链路带出去

```go
req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
obs.InjectHTTP(ctx, req.Header)      // 写入 traceparent
```

接收侧（用 matex 的 `httpx`）无需任何代码：入站 `traceparent` 会自动被提取，服务内
span 续接同一条链路。

### 4. 日志自动对齐

不用改任何日志调用。开了 tracing 后 `obs.TraceID(ctx)` 返回的是当前 span 的 trace id
（32 位 hex），日志里的 `trace_id` 字段自动跟着变，可以直接拿去追踪后端查。

## 约定（必须遵守）

- **span 名用低基数**：`"thing.load"`、路由模板，不要拼具体 id/路径。每个 URL 一个
  span 名会把后端打爆。
- **不写条件分支**：不要 `if tracingEnabled { ... }`，`StartSpan`/`InjectHTTP` 关闭时
  就是 no-op。
- **`defer span.End()`** 紧跟 `StartSpan`，别漏掉。
- 不要自己 `otel.SetTracerProvider` —— 会覆盖框架的 provider；用配置段。
- 采样交给配置（`sample_ratio`），不要在业务里判断。

## 常见问题

| 现象 | 原因 |
|---|---|
| 日志里有 trace_id，但追踪后端查不到 | `endpoint` 没配，或 collector 地址不通 |
| 下游服务的 span 是新的根链路 | 调下游时漏了 `obs.InjectHTTP`，或没把 ctx 传进 request |
| 子 span 和父 span 平级 | `StartSpan` 返回的 ctx 被丢掉了，仍用旧 ctx |
| 关了 tracing 后日志 trace_id 变成 8 位 | 正常：回落到 `X-Request-ID` 生成值 |

## 验证

```sh
go test ./pkg/core/obs ./pkg/core/httpx ./examples/observability
make ci
```

参考 `examples/observability`（含开关两种状态的对比测试）。
