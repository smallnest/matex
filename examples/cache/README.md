# cache — 缓存模式（cache）

`pkg/core/cache` 补的是 **redis 客户端给不了的那两个模式** —— 它们只在压力下才暴露：

| 失效模式 | 现象 |
|---|---|
| **击穿 / 惊群**（stampede） | 一个热 key 到期，所有在途请求同时回源，DB 吃到本该被缓存挡下的那波流量 |
| **同时过期**（synchronized expiry） | 一批一起写入的 key 一起到期，按时钟周期性地制造上面那波流量 |

`GetOrLoad` 一次解决两个：同一 key 的并发 miss 合并成**一次**回源；每条 TTL 都**随机摊开**。

## 运行（需要 redis：`make dev`）

```sh
go run ./examples/cache

curl -s localhost:8083/api/v1/users/1        # miss：~50ms，回源一次
curl -s localhost:8083/api/v1/users/1        # hit：很快，不回源
curl -s localhost:8083/api/v1/stats          # 看回源了多少次

# 并发 miss 合并成一次回源
for i in $(seq 1 20); do curl -s -o /dev/null localhost:8083/api/v1/users/2 & done; wait
curl -s localhost:8083/api/v1/stats          # source_reads 只 +1

# 写后失效：下一次读重新回源
curl -s -X PUT localhost:8083/api/v1/users/1 -d '{"name":"alice v2"}'

go test ./examples/cache
```

## 用法

```go
u, err := cache.GetOrLoad(ctx, s.redis, cache.Key("user", id),
	cache.Config{TTL: 30 * time.Second, Jitter: 5 * time.Second},
	func(ctx context.Context) (User, error) {
		return dao.ByID(ctx, env.DB, id)   // 回源：DB / 下游 / 计算
	})
```

- **`Jitter` 建议取 TTL 的 10~20%**。`Jitter >= TTL` 时会被压到 `TTL/2`（不会抖到 0 ——
  在 redis 里 0 过期等于立即删除）。
- **nil client 不是错误**：不配 redis 就直接回源、不缓存。调用点因此不用写
  `if env.Redis != nil`。
- **回源失败不写缓存**：错误会被原样返回，下一次请求重新尝试。
- **失败不是错误，只是慢**：写缓存失败只记 warn，值已经在手上，不影响正确性。

## 并发合并的取舍

回源跑在**第一个调用者的 ctx** 下。第一个调用者放弃时，这一组合并的请求会一起失败 ——
这是刻意的：另一种做法是让一个调用者早就跑掉的请求继续占着数据库连接。

所以：**别把 GetOrLoad 回源挂在一个随时会取消的短 ctx 上**。

## 写后失效

```go
if err := cache.Invalidate(ctx, s.redis, cache.Key("user", id)); err != nil {
	obs.Warn(ctx, "invalidating the cache failed", "id", id, "err", err)
}
```

**删除而不是更新**。并发写者下，先写 DB 再写缓存有窗口会被旧值覆盖；先写 DB 再删缓存
则任何顺序都安全（下一次读必然回源拿到新值）。

失效失败**不要**让写请求失败 —— 但也不能完全不管：它意味着在 TTL 内可能读到旧值。
记 warn，并把 TTL 当作"最坏情况下的脏读窗口"来设。

## Key 的命名空间

```go
cache.Key("user", 42)          // "user:42"
cache.Key("order", id, "items") // "order:9:items"
```

**必须带命名空间**：缓存 key 和服务的其它 redis 数据共用同一个库，而且 `GetOrLoad`
是按**字面 key** 合并的 —— 两个功能都用 `"1"` 会互相串。

## 没做的事

`cache` 只有一层 redis，**不做本地多级缓存**。本地缓存要额外引入"多实例失效"问题
（一个实例改了数据，其它实例的本地副本还是旧的），收益是省一次网络往返 —— 除非有真实
的压测数据支撑，否则不值得。

## 约定

- 回源范围要小：只包住**那一次**查询，别把整个 handler 塞进去。
- TTL 不是拍脑袋定的：它等于"数据可以有多旧"的业务答案。
- 不缓存不可变值之外的东西要配合 `Invalidate` —— 只写不删的缓存最终会变成 bug 来源。
