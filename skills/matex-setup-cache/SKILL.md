---
name: Matex Setup Cache
description: 在 matex 中接入缓存模式：cache-aside 回源、并发 miss 合并（防击穿）、TTL 抖动（防雪崩）、写后失效。当用户说"加缓存 / 缓存模式 / cache-aside / 防击穿 / 防雪崩 / 缓存击穿 / 缓存穿透 / 缓存一致性 / setup cache"时使用。
---

# 接入缓存

用 `pkg/core/cache`（不是直接 `redis.GetJSON`/`SetJSON`）—— 它补的是两个只在压力下才
暴露的失效模式：**并发 miss 一起回源**（击穿）和**一批 key 同时过期**（雪崩）。

## 1. 读：GetOrLoad

```go
u, err := cache.GetOrLoad(ctx, s.redis, cache.Key("user", id),
	cache.Config{TTL: 30 * time.Second, Jitter: 5 * time.Second},
	func(ctx context.Context) (User, error) {
		return s.dao.ByID(ctx, q, id)   // 回源：DB / 下游 / 计算
	})
```

一次调用做四件事：查缓存 → 命中返回 → miss 时**按 key 合并并发请求**（只回源一次）→
回源结果按**抖动后的 TTL** 写入。

- `Jitter` 取 TTL 的 10~20%；`Jitter >= TTL` 会被压到 `TTL/2`（不会抖成 0）。
- `s.redis` 为 nil 时直接回源不缓存 —— **不用写 `if env.Redis != nil`**。
- 回源报错不写缓存；写缓存失败只记 warn（值已在手上）。

**回源跑在第一个调用者的 ctx 下**：别挂在一个随时取消的短 ctx 上，否则一组请求会一起挂。

## 2. 写：Invalidate

```go
if _, err := s.dao.Update(ctx, env.DB, id, in); err != nil {
	return nil, err
}
if err := cache.Invalidate(ctx, s.redis, cache.Key("user", id)); err != nil {
	obs.Warn(ctx, "invalidate failed", "id", id, "err", err)
}
```

**删缓存，不要更新缓存**：并发写下"写 DB + 写缓存"存在旧值覆盖新值的窗口；"写 DB +
删缓存"任何顺序都安全（下次读必然回源）。

失效失败**不要让写请求失败**，但要记 warn —— 它意味着 TTL 内可能读到旧值。TTL 就是
"最坏情况的脏读窗口"。

## 3. Key 命名空间

```go
cache.Key("user", 42)           // "user:42"
cache.Key("order", id, "items") // "order:9:items"
```

**必须带命名空间**：缓存和其它 redis 数据共用一个库，而 `GetOrLoad` 按**字面 key** 合并
—— 两个功能都用 `"1"` 会互相串。

## 约定（必须遵守）

- **回源范围小**：只包住那一次查询，不要包整个 handler。
- **TTL 是业务答案**，不是拍脑袋的数字：它等于"这份数据可以有多旧"。
- **本地多级缓存不要加**（除非有压测数据）：多实例失效问题比省下的那次往返贵得多。
- 只写不删的缓存最终一定会变成 bug 来源 —— 有写路径就要有对应的 `Invalidate`。

## 测试（无 Docker）

```go
rc := redistest.Start(t)   // miniredis 进程内
svc.redis = rc
```

要断言"并发 miss 只回源一次"，回源函数里加计数器，并让它在 channel 上阻塞到所有
goroutine 都进入 —— 参考 `examples/cache` 的 `TestConcurrentMissesCollapse`。

## 验证

```sh
go test ./pkg/core/cache ./examples/cache
make ci
```
