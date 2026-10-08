---
name: Matex Setup Resilience
description: 在 matex 中给服务加稳定性防护：接口限流（本地/redis）、写请求幂等、下游熔断、退避重试。当用户说"加限流 / 限流 / rate limit / 防刷 / 幂等 / idempotency / 重复提交 / 熔断 / circuit breaker / 重试 / retry / 退避 / 稳定性 / 防护"时使用。
---

# 接入稳定性防护

四个工具，两个作用点。别一次全上 —— 按需要选。

| 工具 | 包 | 作用点 | 何时用 |
|---|---|---|---|
| 限流 | `pkg/core/ratelimit` | httpx 中间件 | 登录、短信、查询类接口怕被刷 |
| 幂等 | `pkg/core/idempotency` | httpx 中间件 | 支付、下单、扣款这类写操作 |
| 熔断 | `pkg/core/breaker` | 出站调用外 | 下游挂了还一直打 |
| 重试 | `pkg/core/retry` | 出站调用外 | 下游偶发抖动 |

## 1. 限流

```go
srv.Use(ratelimit.Middleware(ratelimit.NewLocal(2, 2), ratelimit.ByIP))
srv.Handle("POST", "/api/v1/login", s.login)
```

- 单实例用 `NewLocal(rate, burst)`；要全集群共享额度用
  `NewRedis(env.Redis, limit, window)`（固定窗口，边界最多 2×limit）。
- key：`ByIP` / `ByHeader("X-API-Key")` / `ByRoute`，或自己写一个；返回 `""` 不限流。
- 注意 `ByIP` 用 `RemoteAddr`，在 LB 后面是代理地址。要信 `X-Forwarded-For` 就显式写
  `ByHeader("X-Forwarded-For")`。
- 超限返回 `429 {"code":42901}`；限流器故障时**放行**。

## 2. 幂等

```go
srv.Use(idempotency.Middleware(idempotency.NewRedis(env.Redis), idempotency.Config{}))
srv.Handle("POST", "/api/v1/payments", s.pay)
```

客户端送 `Idempotency-Key`（建议用 UUID）。语义：

- 首次执行并存储结果；同 key 后续请求**重放**该结果；
- 首次还在执行时来的重复请求 → `409 {"code":40902}`；
- 首次是 **5xx** → 释放 key（允许重试成功）；4xx 和成功 → 重放。

默认只覆盖 POST/PATCH/DELETE。`NewMemory()` 只适合单实例/测试，多副本必须 `NewRedis`。

写操作**必须**配合这个，而不是赌重试无害。

## 3. 熔断 + 4. 重试：顺序和参数

```go
v, err := retry.DoValue(ctx, retry.Config{
    Attempts:  3,
    BaseDelay: 50 * time.Millisecond,
    Retryable: func(err error) bool { return !errors.Is(err, breaker.ErrOpen) },
}, func(ctx context.Context) (string, error) {
    return breaker.DoValue(ctx, s.circuit, func(context.Context) (string, error) {
        return s.downstream.call(ctx)
    })
})
if errors.Is(err, breaker.ErrOpen) {
    return nil, errs.Unavailable(50302, "downstream is unavailable")
}
```

两条必须记住的：

- **retry 在外、breaker 在内**，并让 `Retryable` 放行 `breaker.ErrOpen` 之外的都重试、
  `ErrOpen` 直接停。反过来会把熔断放大成重试风暴。
- **熔断阈值 ≥ Attempts**（示例：`Failures: 3` 对 `Attempts: 3`）。阈值小于重试次数时，
  熔断器会在重试循环中间打开，最后一次尝试连机会都没有 —— 这个 bug 只在有抖动的环境下
  才暴露，容易漏掉。

`retry` 默认全抖动（把延迟摊到 `[0, d)`），不要关掉：同一瞬间失败的一批客户端若无抖动
会在同一瞬间重试。

## 约定（必须遵守）

- **降级优先**：限流器/幂等存储故障时放行 + 记 warn，绝不因为防护组件挂掉而拒绝正常流量。
- **包裹范围小**：breaker/retry 只包住那一次出站调用，不要包整个 handler。
- **只重试幂等操作**：写操作配 `idempotency`。
- 熔断的 `OnStateChange` 在锁内调用：只记日志/打点，别回调 Breaker。

## 验证

```sh
go test ./pkg/core/ratelimit ./pkg/core/breaker ./pkg/core/retry ./pkg/core/idempotency
go test ./examples/resilience
make ci
```

参考 `examples/resilience`（四个工具在一条链路上的集成测试，含"熔断打开后不再调用下游"
的断言）。
