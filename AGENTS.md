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
pkg/core/                 基础设施：config/db/redis/memcache/kafka/httpx/obs/errs/verticle/app/feature/audit
configs/config.yaml       配置（框架段 + 每服务一段，段 key = Service.Name()）
migrations/*.sql          数据库迁移（文件名序执行，追加不修改）
examples/<feature>/       每个能力一个可运行示例（离线可跑、带测试；见 examples/README.md）
skills/                   AI skill（matex-add-domain、matex-setup-*）
```

## 关键概念

- **verticle.Service**：部署单元 = 一个 `Name()` + `Setup(ctx, env)` + `BuildRouter(srv)`。`verticle.Run` 统一驱动：加载配置 → 初始化基础设施（配了才建）→ Setup → 建 HTTP server → BuildRouter → 运行 → 优雅退出。基础设施句柄对业务是**私有**的，一律经 `Env` 访问器读取：`RequireDB()`（/`RequireRedis`/`RequireMemcache`/`RequireKafka`）在未配置时让启动失败，`DB()`（/`Redis`/`Memcache`/`Kafka`）逗号-ok 判可用。不写 `if env.DB != nil`。
- **配置热更**：实现 `verticle.ConfigWatcher` 接口，`OnServiceConfigChange(raw map[string]any)` 会收到服务段新内容。
- **跨进程追踪自动生效**：HTTP（obs）、gRPC（grpcx 拦截器）、rpcx（plugin + 客户端包装）都自动传播 W3C traceparent，业务零代码；关闭 tracing 时全为 no-op。
- **功能开关**：`feature.Register(Flag{...})` 声明，`Flag.Enabled()` 在**每个请求处**读；`verticle` 把 `feature:` 配置段灌入，`FEATURE_<NAME>` 环境变量与 `feature.Set` 更高优先。
- **审计**：业务动作成功后（含 4xx 拒绝）`audit.Record(ctx, audit.Event{...})`；默认写结构化日志，需要落库时 `audit.SetSink`。
- **熔断两种模型**：推荐 `Ratio/Window/MinSamples`（错误率滑窗，4xx 不计失败）；不设 `Ratio` 时为旧的连续 `Failures` 计数。
- **Kafka 毒消息**：需要留证据/可重放时用 `RunConsumerDLQ` + `kafka.dlq_topic`。
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
- 接认证/授权 → `matex-setup-auth`
- 接链路追踪 → `matex-setup-tracing`
- 加限流/幂等/熔断/重试 → `matex-setup-resilience`
- 接数据库 → `matex-setup-database`
- 加缓存（防击穿/防雪崩）→ `matex-setup-cache`
- 加定时任务（多副本选主）→ `matex-setup-cron`
- 接 redis → `matex-setup-redis`
- 接 memcache → `matex-setup-memcache`
- 接 kafka（生产者/消费者）→ `matex-setup-kafka`
- 接 gRPC → `matex-setup-grpc`
- 接 rpcx → `matex-setup-rpcx`
- 加功能开关 → `matex-setup-feature`
- 加审计日志 → `matex-setup-audit`

## 示例

`examples/<feature>/` 每个能力一个可运行示例，一律遵守上面的约定（只经 `pkg/core/*`，
不 import 驱动库，depguard 的 `examples-no-drivers` 强制；唯一豁免是 kafka 示例测试用的
`franz-go/pkg/kfake`）。示例要能**离线跑 + 带测试**（SQLite / miniredis / kfake / 进程内
gRPC·rpcx），所以 `make ci` 就能验证它们。改完务必 `make ci` 通过再交付。
