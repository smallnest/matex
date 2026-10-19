# redis — 缓存与分布式锁

`pkg/core/redis` 包住 go-redis：`Open` 会 ping 一次（连不上就在启动时失败），
并提供 JSON 读写、阻塞可重入的 `Lock` 与一次性 `TryLock`。业务代码不直接 import go-redis。

## 运行

```sh
make dev                      # 起 redis（以及其它中间件）
go run ./examples/redis

curl -i localhost:8080/api/v1/cache/world          # cached=false（miss，回填）
curl -i localhost:8080/api/v1/cache/world          # cached=true
curl -i -X DELETE localhost:8080/api/v1/cache/world

# 阻塞等待（wait_timeout 内没拿到就 409）+ 重入 + 看门狗续租
curl -i -X POST localhost:8080/api/v1/lock/job1 & curl -i -X POST localhost:8080/api/v1/lock/job1
# 一次尝试，忙则立即 409
curl -i -X POST localhost:8080/api/v1/trylock/job1 & curl -i -X POST localhost:8080/api/v1/trylock/job1

go test ./examples/redis       # 用 miniredis，离线可跑
```

## 缓存：cache-aside

```go
e, ok, err := redis.GetJSON[Entry](ctx, rc, key)   // miss 时 ok=false，不是 error
if ok { return e.Value, true, nil }

e = compute(key)
_ = rc.SetJSON(ctx, key, e, ttl)                   // 回填 + TTL
return e.Value, false, nil
```

注意 **`GetJSON` 是包级泛型函数**（`redis.GetJSON[T](ctx, c, key)`），`SetJSON` 是方法。
`ok=false` 表示 miss；`err` 表示 redis 出错或内容损坏——**要不要把损坏当 miss 是你自己的策略**。

## 分布式锁：TryLock（一次性）

```go
release, ok, err := rc.TryLock(ctx, key, ttl)
if err != nil { return err }
if !ok { return errs.Conflict(40901, "locked by someone else") }
defer release(ctx)     // 只有仍持有锁时才会删 key（Lua 校验 token）
```

- 锁是**带 TTL 的 SETNX**：持有者崩了会自动释放，不会死锁。
- `release` 用 Lua 比对 token 再删，避免"锁过期后被别人拿到，自己却把它删了"。
- `TryLock` 只做一次尝试，用在你"宁可跳过也不排队"的场景。

## 分布式锁：Lock（阻塞 + 重入 + 续租）

```go
l, err := rc.Lock(ctx, key,
    redis.WithLockTTL(5*time.Second),    // 租期
    redis.WithWaitTimeout(2*time.Second),// 阻塞等待上限，超时返回 ErrLockBusy
    redis.WithWatchdog(time.Second),      // 看门狗每 watchdog/3 续租一次
)
if errors.Is(err, redis.ErrLockBusy) { /* 409 */ }
defer l.Release(ctx)

// 重入：同一个客户端再次 Lock 同一个 key，立即返回同一个 l，计数 +1
inner, _ := rc.Lock(ctx, key)
defer inner.Release(ctx)   // 每层 Lock 对应一层 Release

<-l.Lost()                 // 租约丢失（failover / 长暂停）时被关闭
```

- **阻塞等待**：忙时排进 Redis 里的 FIFO 队列，队头才有资格抢；`WithWaitTimeout`
  到期或 ctx 取消会离队并返回。
- **可重入**：同一个 `*Client` 对同一个 key 再次 `Lock` 是重入而非自等；
  `Held()` 返回当前层数，每层 `Lock` 对应一次 `Release`。
- **自动续租**：`WithWatchdog` 打开后看门狗每 `watchdog/3` 续租，临界区可比
  `LockTTL` 长；续租失败时 `Lost()` 关闭、`Renew` 返回 `ErrLockLost`。
- 锁在 Redis 里的实际 key 是 `lock:<key>`，和一个 `<key>:queue` 等待队列，
  与 `TryLock`（直接用原始 key）互不干扰。

## key 设计

用配置里的 `prefix`（如 `example:`）把业务 key 收拢，方便和别的应用共用实例、
按前缀扫 key。缓存 key 和锁 key 分开命名（示例：`example:lock:<name>`）。

## 想要完整 Redis API

`rc.Cmdable()` 返回 `goredis.Cmdable`，管道、Lua、hash/set/zset 都在那里：

```go
pipe := rc.Cmdable().Pipeline()
pipe.Incr(ctx, "counter"); pipe.Expire(ctx, "counter", time.Hour)
_, _ = pipe.Exec(ctx)
```
