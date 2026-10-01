---
name: Matex Setup Memcache
description: 在 matex 中接入 Memcached：配置 memcache 段、JSON 读写。当用户说"接 memcache / 配 memcached / setup memcache / 加 memcached / 内存缓存"时使用。
---

# 接入 Memcached

Memcached 经 `pkg/core/memcache` 使用（gomemcache 薄封装 + JSON 助手），业务不 import gomemcache。适合存热点、计数、会话等"可丢"的数据。

## 步骤

### 1. 配置（configs/config.yaml）

```yaml
memcache:
  addrs: ["localhost:11211"]
  timeout: 300ms
  max_idle: 16
```

留空/删除 `memcache:` 段即禁用（`env.Memcache` 为 nil）。本地 `make dev` 起 memcached。

### 2. 读写

```go
func (s *Service) Get(ctx context.Context, id int64) (*Thing, error) {
	key := fmt.Sprintf("thing:%d", id)
	if s.deps.MC != nil {
		if v, ok, err := memcache.GetJSON[Thing](s.deps.MC, key); err == nil && ok {
			return &v, nil
		}
	}
	t, err := s.load(ctx, id)   // 回源
	if err != nil { return nil, err }
	if s.deps.MC != nil {
		_ = s.deps.MC.SetJSON(key, t, 5*time.Minute)
	}
	return t, nil
}
```

`memcache.GetJSON[T]` 返回 `(v, ok, err)`，`ok=false` 是 miss（`memcache.ErrMiss` 已归一）。

## 约定

- memcached 无持久化、无复杂结构，只存 JSON blob；key 用命名空间前缀。
- 过期时间是秒粒度（`ttl.Seconds()`），写缓存失败只记日志。
- 与 redis 二选一时：需要 TTL 精确、原子操作、锁 → redis；纯热点可丢 → memcache。

## 验证

```sh
make ci
```
