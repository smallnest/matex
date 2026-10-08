# memcache — Memcached 缓存

`pkg/core/memcache` 包住 gomemcache。memcached 协议没有 ping，所以 `Open` 不会
在启动时探测连通性——**第一条命令才会失败**，这一点和 redis 不同。

## 运行

```sh
make dev
go run ./examples/memcache
go run ./examples/memcache -addrs '10.0.0.1:11211,10.0.0.2:11211' -key other -ttl 5s
```

## 用法

```go
c := memcache.Open(memcache.Config{
    Addrs:   []string{"localhost:11211"},   // 多地址 = 客户端分片
    Timeout: time.Second,
    MaxIdle: 16,
})

err := c.SetJSON(key, v, 60*time.Second)          // 方法
v, ok, err := memcache.GetJSON[T](c, key)          // 包级泛型函数；ok=false 即 miss
err  = c.Touch(key, 5*time.Minute)                 // 只续期，不改值
err  = c.Delete(key)                               // 删不存在的 key 也不报错
```

**Memcached 没有 context**（协议本身没有），超时设置在 client 上。

## 什么时候用 memcache 而不是 redis

- 要的是**纯内存、多实例共享**的短命缓存，不关心持久化、不关心数据结构；
- 已经是 memcached 生态。

其它情况优先 `pkg/core/redis`：它有 TTL、分布式锁、pipeline、Lua，语义更全。

## 和 redis 一样的约定

`GetJSON` 的 miss 是 `ok=false`；只有网络错误或内容损坏才返回 `err`。TTL 精度是**秒**，
传 500ms 实际按 0 处理（等于立刻过期），所以 TTL 不要小于 1s。
