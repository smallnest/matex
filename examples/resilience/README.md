# resilience — 稳定性防护

四个工具，两个作用点：

| 工具 | 作用点 | 防的是什么 |
|---|---|---|
| `ratelimit` | httpx 中间件（入站） | 一个调用方打得太快 —— **保护自己** |
| `idempotency` | httpx 中间件（入站） | 同一个写请求来了两次 —— **保护数据** |
| `breaker` | 包住出站调用 | 一直打一个已经挂掉的下游 —— **保护它** |
| `retry` | 包住出站调用 | 瞬时失败 —— **把它藏起来** |

## 运行

```sh
go run ./examples/resilience

# 限流：第三个快速请求被 429 挡掉
for i in 1 2 3; do curl -s -o /dev/null -w '%{http_code} ' \
  -X POST localhost:8082/api/v1/login -d '{"user":"alice"}'; done; echo

# 幂等：两个请求返回同一个 payment id，只创建一次
curl -s -X POST localhost:8082/api/v1/payments -H 'Idempotency-Key: k-1' -d '{"amount":10}'
curl -s -X POST localhost:8082/api/v1/payments -H 'Idempotency-Key: k-1' -d '{"amount":10}'

# 重试 + 熔断：第一次请求耗尽重试预算，之后立即失败
curl -s localhost:8082/api/v1/flaky/1
curl -s localhost:8082/api/v1/flaky/2

go test ./examples/resilience
```

## 限流

```go
srv.Use(ratelimit.Middleware(ratelimit.NewLocal(rate, burst), ratelimit.ByIP))
srv.Handle("POST", "/api/v1/login", s.login)
```

- `NewLocal(rate, burst)`：进程内按 key 的令牌桶。空闲够久的 key 会被丢掉，所以 IP
  这种无界 key 空间不会把内存撑爆。**每个实例各算各的**。
- `NewRedis(client, limit, window)`：**固定窗口**，整个集群共享一份计数。代价是窗口
  边界最多能过 2×limit，换来一次往返 + 原子性。
- key 用 `ByIP` / `ByHeader("X-API-Key")` / `ByRoute` 或自己写；返回 `""` 表示不限流。

限流器本身故障时**放行**（记 warn）：丢掉限流是坏事，但为此拒掉所有正常流量更坏。

`ByIP` 用的是 `RemoteAddr` —— 在负载均衡后面那是代理的地址。要信 `X-Forwarded-For`
就显式写 `ByHeader("X-Forwarded-For")`，别默认信任。

## 幂等

```go
srv.Use(idempotency.Middleware(idempotency.NewRedis(env.Redis), idempotency.Config{}))
srv.Handle("POST", "/api/v1/payments", s.pay)   // 客户端带 Idempotency-Key
```

流程：第一个请求占住 key 并执行 → 结果存下来；带同一个 key 的后续请求**重放**那个结果，
不再执行。第一个还在跑时来的重复请求返回 **409**（`"code":40902`），而不是并行跑第二遍 ——
那正是这个 key 存在的意义。

| 首次结果 | 后续同 key 的请求 |
|---|---|
| 成功（含 204） | 重放相同响应 |
| 4xx（确定性拒绝） | 重放相同错误 |
| **5xx** | **释放 key**，允许重试成功 |

最后一行是刻意的：5xx 是我们自己的坏时刻，不该被钉在 key 上重放整个 TTL。

默认只对 `POST`/`PATCH`/`DELETE` 生效（GET 本来就幂等）。存储故障时**放行**。

`NewMemory()` 只适合单实例和测试 —— 多副本要用 `NewRedis`，否则每个实例只去重自己那份流量。

## 重试 + 熔断：顺序很重要

```go
payload, err := retry.DoValue(ctx, retry.Config{
    Attempts:  3,
    BaseDelay: 5 * time.Millisecond,
    Retryable: func(err error) bool {
        return !errors.Is(err, breaker.ErrOpen)   // 熔断了就别再重试
    },
}, func(ctx context.Context) (string, error) {
    return breaker.DoValue(ctx, s.circuit, func(context.Context) (string, error) {
        return s.downstream.call()
    })
})
```

**retry 在外、breaker 在内**：每次尝试都过一遍熔断；熔断打开后重试循环立刻停，
不再补上三次注定失败的调用。

两个必须踩到的坑：

1. **熔断阈值 ≥ 重试次数**。否则熔断器会在重试循环中间打开，最后一次尝试连机会都没有。
   示例里 `Failures: 3` 对应 `Attempts: 3`，是最紧的配置。
2. **延迟必须带抖动**。`retry` 默认全抖动（full jitter）：把延迟摊到 `[0, d)`，否则
   同一瞬间失败的一批客户端会在同一瞬间重试，把小抖动放大成尖峰。

`retry` 只对幂等操作安全。写操作要配合 `idempotency`，而不是赌第二次无害。

## 约定

- 入站防护（限流、幂等）挂 `httpx` 中间件，靠 `Use` 的累积语义叠层级。
- 出站防护（重试、熔断）包在**具体那一次调用**外面，包裹范围要小 —— 包住整个 handler
  会让熔断器统计到一堆无关的失败。
- 每个工具的失败策略都是**宁可降级、不可熔断主流程**：限流器和幂等存储出问题都放行，
  只记日志。
