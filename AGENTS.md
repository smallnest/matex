# AGENTS.md — matex 开发指南

matex 是一个**大道至简**的企业级 Go 脚手架：零 Web 框架、零 ORM，用标准库 + 少量驱动库薄封装。它不做"全家桶"，而是把最常用的布局和基础设施入口固化下来，把"生成代码"这件事交给 AI（也就是你）和 skills 完成。

## 核心原则（任何改动前先读）

1. **业务代码不 import 驱动库**（pgx / go-sql-driver/mysql / modernc.org/sqlite / go-redis / franz-go / gomemcache / gin / echo …），一律经 `pkg/core/*`。这由 `.golangci.yml` 的 depguard 强制。
2. **HTTP handler 不碰 ResponseWriter**，签名统一为 `func(ctx, *http.Request) (any, error)`，返回 nil 即 204，返回 `errs.*` 即对应状态码 + `{code,msg,data}`。
3. **配置用 tag**：`default:` / `env:` / `optional`，见 `pkg/core/config`。
4. **错误用 `errs`**：`errs.NotFound(40401, "user %d not found", id)`。
5. **日志用 `obs`**：`obs.Info(ctx, "msg", "k", v)`，ctx 带 trace id。

## 布局

```
cmd/<service>/            一个二进制一个部署单元
internal/app/             实现 verticle.Service（Name/Setup/BuildRouter）
internal/domain/<name>/   业务域：handler.go（HTTP 边界）+ service.go（业务）+ dao.go（数据）
pkg/core/                 基础设施：config/db/redis/memcache/kafka/httpx/obs/errs/verticle/app
configs/config.yaml       配置（框架段 + 每服务一段，段 key = Service.Name()）
migrations/*.sql          数据库迁移（文件名序执行，追加不修改）
examples/<feature>/       每个能力一个可运行示例（离线可跑、带测试；见 examples/README.md）
skills/                   AI skill（matex-add-domain、matex-setup-*）
```

## 关键概念

- **verticle.Service**：部署单元 = 一个 `Name()` + `Setup(ctx, env)` + `BuildRouter(srv)`。`verticle.Run` 统一驱动：加载配置 → 初始化基础设施（配了才建，没配为 nil）→ Setup → 建 HTTP server → BuildRouter → 运行 → 优雅退出。`env.DB/env.Redis/...` 为 nil 时表示未配置，代码要 `if env.X != nil` 防御。
- **配置热更**：实现 `verticle.ConfigWatcher` 接口，`OnServiceConfigChange(raw map[string]any)` 会收到服务段新内容。
- **测试无 Docker**：`dbtest.Start(t)`（默认纯 Go SQLite + 自动迁移，离线可用；`DB_TEST_DRIVER/DSN` 可切真库）、`redistest.Start(t)`（miniredis）。

## 常用命令

```sh
make build         # 编译
make run           # 本地运行（先注释掉 config.yaml 里 db/redis 段即可零依赖启动）
make test-short    # 快速测试（含 sqlite DB 用例，无需外部依赖）
make test          # 全量测试
make ci            # vet + test-short + build
make dev           # 起本地 postgres/mysql/redis/memcache/kafka（docker compose）
make migrate DSN=postgres://...            # 默认 postgres；-driver 可切 sqlite/mysql
```

## 加东西时按 skill 走

- 新增业务域 → `matex-add-domain`
- 接数据库 → `matex-setup-database`
- 接 redis → `matex-setup-redis`
- 接 memcache → `matex-setup-memcache`
- 接 kafka（生产者/消费者）→ `matex-setup-kafka`
- 接 gRPC → `matex-setup-grpc`
- 接 rpcx → `matex-setup-rpcx`

## 示例

`examples/<feature>/` 每个能力一个可运行示例，一律遵守上面的约定（只经 `pkg/core/*`，
不 import 驱动库，depguard 的 `examples-no-drivers` 强制；唯一豁免是 kafka 示例测试用的
`franz-go/pkg/kfake`）。示例要能**离线跑 + 带测试**（SQLite / miniredis / kfake / 进程内
gRPC·rpcx），所以 `make ci` 就能验证它们。改完务必 `make ci` 通过再交付。
