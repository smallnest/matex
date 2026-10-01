# matex

**大道至简**的企业级 Go 脚手架，AI 原生。

零 Web 框架、零 ORM、零微服务全家桶——只保留最常用的东西：一个清晰的布局、一套薄封装的基础设施（PostgreSQL / MySQL / SQLite / Redis / Memcached / Kafka / 配置），以及一套教 AI（和队友）正确使用它们的 **skills**。代码生成不是交给一个 CLI，而是交给 AI + skill。

> 设计取舍见 [docs/design.md](docs/design.md)。

## 特性

- **最常用布局**：`cmd/` → `internal/app/`（服务装配）→ `internal/domain/`（业务域）→ `pkg/core/`（基础设施）。
- **基础设施一键初始化**：数据库、Kafka、Redis、Memcached、配置——`config.yaml` 里配了哪个就初始化哪个，没配的对应依赖为 `nil`（代码 `if env.X != nil` 防御）。
- **Verticle 服务抽象**：一个部署单元 = `Name()` + `Setup(ctx, env)` + `BuildRouter(srv)`，`verticle.Run` 统一驱动生命周期与优雅退出，配置热更可插拔。
- **零 Web 框架**：标准库 `net/http` + Go 1.22 ServeMux（方法路由 + 路径参数），`pkg/core/httpx` 统一处理 recover / 超时 / trace id / 访问日志 / metrics / JSON 响应与错误映射。
- **AI skills**：顶层 `skills/` 目录（`matex-` 前缀命名，`npx skills add smallnest/matex --all` 可一次全装）内置 7 个 skill，新增 domain、接数据库/redis/memcache/kafka/grpc/rpcx 都有规可依（`AGENTS.md` 是总入口）。
- **测试无 Docker、离线可用**：`dbtest`（默认纯 Go SQLite + 自动迁移，`DB_TEST_DRIVER/DSN` 可切真库）、`redistest`（miniredis）。

## 快速开始

```sh
# 1. 零依赖直接跑（只用 configs/config.min.yaml：http + log + 服务段）
make run-min
curl -i localhost:8080/healthz
curl -i localhost:8080/api/v1/hello/world

# 2. 带数据库、但零外部依赖（纯 Go SQLite，无 cgo / 无 Docker）
make migrate DRIVER=sqlite DSN=file:matex.db
make run-sqlite

# 3. 带真依赖跑（可选）
make dev                          # 起 postgres/mysql/redis/memcache/kafka
make migrate DSN=postgres://matex:matex@localhost:5432/matex?sslmode=disable   # 默认 postgres；DRIVER 可切 sqlite/mysql
make run
```

配置分三份：`configs/config.min.yaml`（零依赖）、`configs/config.sqlite.yaml`（带 SQLite 库、零外部依赖）、`configs/config.yaml`（含 db/redis/memcache/kafka 段，各段可选——配了才初始化）。

## 布局

```
cmd/<service>/            一个二进制一个部署单元
internal/app/             实现 verticle.Service（Name/Setup/BuildRouter），装配 domain
internal/domain/<name>/   业务域：handler.go（HTTP 边界）+ service.go（业务）+ dao.go（数据）
pkg/core/                 基础设施：config / db / redis / memcache / kafka / httpx / obs / errs / verticle / app
configs/config.yaml       配置（框架段 + 每服务一段，段 key = Service.Name()）
migrations/*.sql          数据库迁移（文件名序执行，追加不修改）
skills/                   AI skill（matex-* 前缀）
.claude-plugin/           skill 插件清单（plugin.json = matex 大伞，一键全装）
```

## Skills

| Skill | 用途 |
|---|---|
| `matex-add-domain` | 新增业务域（handler/service/dao + 路由 + 装配 + 测试） |
| `matex-setup-database` | 接数据库 PostgreSQL / MySQL / SQLite（配置 / 迁移 / DAO / dbtest） |
| `matex-setup-redis` | 接 Redis（缓存 / 分布式锁） |
| `matex-setup-memcache` | 接 Memcached |
| `matex-setup-kafka` | 接 Kafka（生产者 / 消费者） |
| `matex-setup-grpc` | 接 gRPC（注册服务 / client） |
| `matex-setup-rpcx` | 接 rpcx（注册服务 / client，无需 IDL） |

## 核心约定

1. 业务代码**不 import 驱动库**（pgx / go-sql-driver/mysql / modernc.org/sqlite / go-redis / franz-go / gomemcache / gin…），一律经 `pkg/core/*`（depguard 强制）。
2. Handler 签名统一 `func(ctx, *http.Request) (any, error)`：返回 `nil` 即 204，返回 `errs.*` 即对应状态码 + `{code,msg,data}`。
3. 配置用 tag：`default:` / `env:` / `optional`；`${VAR}` / `${VAR:default}` 环境变量展开。
4. 错误用 `errs`，日志用 `obs`（ctx 带 trace id）。

## 常用命令

```sh
make build / make run / make bins       # 编译、运行、打二进制
make test / make test-short             # 全量 / 快速测试（均含 sqlite DB 用例）
make ci                                 # vet + test-short + build
make dev / make dev-down                # 起 / 停本地依赖
make migrate DSN=postgres://...          # 迁移（默认 postgres；DRIVER=sqlite|mysql 可切）
make run-sqlite                          # 带 SQLite 库、零外部依赖运行
make lint / make fmt / make tidy
make docker TAG=v1.0.0                  # 镜像
```

## 设计取舍

- **不用 ORM**：SQL 直写 + `*db.DB.QueryOne[T]` 泛型扫描（postgres / mysql / sqlite 三驱动，`?` 占位符统一），简单、可调试、可 review。
- **不用 gin/echo**：ServeMux 够用，`httpx` 补齐统一响应与观测。
- **不做服务发现/熔断/限流**：这些归 K8s + 网关；服务内只保留超时、recover、优雅退出（见 [docs/design.md](docs/design.md) 的论证）。
- **代码生成交给 AI**：skills 就是"生成器"，且不用维护一个生成器二进制。
