---
name: Matex Add Domain
description: 在 matex 脚手架中新增一个业务域（handler/service/dao + 路由注册 + 装配 + 测试）。当用户说"新增 domain / 加一个业务域 / 新增模块 / add domain / new domain / 建一个 xx 服务"时使用。
---

# 新增业务域

在 `internal/domain/<name>/` 下新增一个业务域，并把它的路由挂到服务上。参考现成的 `internal/domain/helloworld`。

## 步骤

### 1. 建包 `internal/domain/<name>/`

创建三个文件（沿用 helloworld 的形态）：

**service.go** — 业务逻辑，`Deps` 结构注入，nil 即"未配置"：

```go
package <name>

import (
	"context"
	"github.com/smallnest/matex/pkg/core/db"
	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/kafka"
)

type Deps struct {
	DB       *db.DB
	Redis    *redis.Client
	Producer *kafka.Producer
	// 其它服务专属依赖
}

type Service struct{ deps Deps }

func New(d Deps) *Service { return &Service{deps: d} }

func (s *Service) Do(ctx context.Context, id int64) (any, error) {
	// 业务：缓存读写、DB、事件发布，每处 if s.deps.X != nil 防御
	return nil, nil
}
```

**handler.go** — HTTP 边界，只做参数解析 + 调 service + 返回：

```go
type Handler struct{ svc *Service }

func (h *Handler) Do(ctx context.Context, r *http.Request) (any, error) {
	id := r.PathValue("id")
	if id == "" {
		return nil, errs.Invalid(40001, "id is required")
	}
	// 需要 body 时用 httpx.ReadJSON(r, &req)，失败返回 errs.Invalid(40002, ...)
	return h.svc.Do(ctx, parseID(id))
}
```

**dao.go** — 数据访问（若用到 DB）：

```go
type DAO struct{ db *db.DB }

func (d *DAO) ByID(ctx context.Context, id int64) (*Row, error) {
	return d.db.QueryOne[Row](ctx, `SELECT ... WHERE id = ?`, id)
}
```

### 2. 在 `internal/app/service.go` 里装配

```go
s.<name> = <name>.New(<name>.Deps{DB: env.DB, Redis: env.Redis, Producer: env.Kafka})

// BuildRouter 里注册路由：
srv.Handle("GET", "/api/v1/<name>/{id}", <name>.NewHandler(s.<name>).Do)
```

### 3. 写测试（无 Docker）

- service 逻辑用 `redistest.Start(t)` 拿 redis；
- dao 用 `dbtest.Start(t)` 拿迁移好的库（默认 SQLite，离线可用；需要新表就加 `migrations/NNN_xxx.sql`）。

## 约定（必须遵守）

- handler 不做业务，service 不碰 HTTP，dao 只写 SQL。
- 所有错误用 `errs`，不裸 `errors.New` 抛给框架。
- 业务代码不 import 驱动库（depguard 会拦），只经 `pkg/core`。
- 路由 method 用大写（`GET`/`POST`…），path 用 Go 1.22 语法（`{id}`）。

## 验证

```sh
make ci
```
