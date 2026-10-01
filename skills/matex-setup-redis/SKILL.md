---
name: Matex Setup Redis
description: 在 matex 中接入 Redis：配置 redis 段、缓存读写、分布式锁。当用户说"接 redis / 配 redis / setup redis / 缓存 / 分布式锁 / 加缓存"时使用。
---

# 接入 Redis

Redis 经 `pkg/core/redis` 使用（go-redis 薄封装 + JSON 助手 + 带 token 的分布式锁），业务不 import go-redis。

## 步骤

### 1. 配置（configs/config.yaml）

```yaml
redis:
  addr: ${REDIS_ADDR:localhost:6379}
  password: ""
  db: 0
```

留空/删除 `redis:` 段即禁用（`env.Redis` 为 nil）。本地 `make dev` 起 redis。

### 2. 缓存读写（cache-aside）

```go
func (s *Service) Get(ctx context.Context, id int64) (*Thing, error) {
	key := fmt.Sprintf("thing:%d", id)
	if s.deps.Redis != nil {
		if v, ok, err := redis.GetJSON[Thing](ctx, s.deps.Redis, key); err == nil && ok {
			return &v, nil   // 命中
		}
	}
	// 回源（DB / 计算）
	t, err := s.load(ctx, id)
	if err != nil { return nil, err }
	if s.deps.Redis != nil {
		_ = s.deps.Redis.SetJSON(ctx, key, t, time.Minute)  // 写缓存
	}
	return t, nil
}
```

`redis.GetJSON[T]` 返回 `(v, ok, err)`：`ok=false` 是 miss，`err!=nil` 是故障/坏值（按需决定当 miss 还是报错）。

### 3. 分布式锁

```go
release, ok, err := s.deps.Redis.TryLock(ctx, "lock:job:1", 10*time.Second)
if err != nil { return err }
if !ok { return nil, errs.Conflict(40900, "job already running") }
defer release(context.Background())  // 只释放自己持有的锁，ttl 兜底防死锁
```

## 约定

- key 用命名空间前缀（如 `matex:thing:1`），集中放一个 `keys.go` 更佳。
- 写缓存失败只记日志不阻断主流程；读缓存失败是否降级回源要显式决策。
- 锁必须带 ttl，`TryLock` 的 release 会校验持有者 token，安全释放。

## 测试（无 Docker）

```go
rc := redistest.Start(t)   // miniredis 进程内
```

## 验证

```sh
make ci
```
