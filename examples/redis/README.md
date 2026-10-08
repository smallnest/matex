# redis — 缓存与分布式锁

`pkg/core/redis` 包住 go-redis：`Open` 会 ping 一次（连不上就在启动时失败），
并提供 JSON 读写与 `TryLock`。业务代码不直接 import go-redis。

## 运行

```sh
make dev                      # 起 redis（以及其它中间件）
go run ./examples/redis

curl -i localhost:8080/api/v1/cache/world          # cached=false（miss，回填）
curl -i localhost:8080/api/v1/cache/world          # cached=true
curl -i -X DELETE localhost:8080/api/v1/cache/world

# 并发的两个请求，第二个 409
curl -i -X POST localhost:8080/api/v1/lock/job1 & curl -i -X POST localhost:8080/api/v1/lock/job1

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

## 分布式锁

```go
release, ok, err := rc.TryLock(ctx, key, ttl)
if err != nil { return err }
if !ok { return errs.Conflict(40901, "locked by someone else") }
defer release(ctx)     // 只有仍持有锁时才会删 key（Lua 校验 token）
```

- 锁是**带 TTL 的 SETNX**：持有者崩了会自动释放，不会死锁。
- `release` 用 Lua 比对 token 再删，避免"锁过期后被别人拿到，自己却把它删了"。
- 需要"必须拿到锁"时自己重试；`TryLock` 只做一次尝试。

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
