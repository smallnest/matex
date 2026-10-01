# matex 设计说明

## 1. 定位：大道至简的企业级脚手架

matex 取**中间偏简**：不做"全家桶"，也不做"迁移专用"，而是把企业级项目里**最常用、最通用**的部分固化下来，把**不通用**的部分明确交给外部（K8s、网关）或 AI（skills）。

## 2. 逐项取舍

### 2.1 用了什么

| 能力 | 做法 |
|---|---|
| 服务抽象 | `verticle.Service`：Name/Setup/BuildRouter + `Run` 统一驱动 + `Env` 注入 |
| HTTP 层 | 标准库 ServeMux + `httpx`：HandlerFunc 返回 `(any, error)`，统一 recover/超时/trace/日志/metrics/JSON |
| 配置 | YAML + `${VAR}` 展开 + `default/env/optional` tag + 热更（fsnotify + 100ms 去抖） |
| 存储 | 三驱动（pgx / go-sql-driver-mysql / modernc-sqlite）统一走 `database/sql` + `?` 占位符重写 + 泛型扫描 + 慢日志 |
| 错误 | `errs`：Kind→HTTP 状态 + 业务 code，`{code,msg,data}` 响应 |
| 观测 | slog JSON + prometheus（`http_server_*` 指标） |
| 生命周期 | `app`：信号 → http 排空 → block 等待 → closer 逆序 |
| 分布式锁 | redis `TryLock`：带 token + Lua 释放 + TTL 兜底 |
| 测试基座 | dbtest（默认纯 Go SQLite + 自动迁移，可切真库）、redistest（miniredis） |
| 一致性守卫 | depguard 两条规则（业务禁驱动库、禁 Web 框架） |
| 代码生成 | **AI + skills**（`skills/` + `AGENTS.md`） |

### 2.2 刻意不做什么（及理由）

| 舍弃 | 理由 |
|---|---|
| ORM | SQL 直写 + `QueryOne[T]` 已够；ORM 增加黑盒与逃逸面。 |
| gin/echo/chi | Go 1.22 ServeMux 原生支持方法路由 + 路径参数，`httpx` 补齐统一响应与观测；再引入第三方路由只会增加依赖面与分叉。 |
| 服务发现 / 注册中心 | K8s 拓扑 + 网关单一入口；静态 DNS + 环境变量足够。 |
| 熔断 / 限流 / 自适应降载 | 上游是 PG/Redis/Kafka，驱动库自带重试与超时；过载保护归网关与 K8s。服务内保留超时预算 + recover 即可。这是"大道至简"的核心裁量——不是不做高可用，而是不在脚手架里堆默认开启的重治理。 |
| OpenTelemetry 全链路 | 迁移/起步期用 trace id（X-Request-ID）贯穿日志已足够；需要时再加 otel（预留了 obs 层）。 |
| 配置热更的设施重建 | 热更只回调服务段配置，不重建 db/redis 连接（避免复杂度爆炸）。 |
| 代码生成器二进制 | 生成器是"会过时的约定"，skill 是"给 AI 的约定"——AI 读 `AGENTS.md` + skill 即可生成符合规范的代码，且模板随仓库演进。 |

## 3. 核心抽象：verticle.Service

```go
type Service interface {
	Name() string                              // 配置段 key、日志/指标前缀
	Setup(ctx context.Context, env *Env) error // 解码服务段、装配 domain
	BuildRouter(s *httpx.Server) error         // 注册路由
}
```

`Run` 的启动序列（每一步 fail-fast）：

```
config.LoadMap → 框架段(ParseMap) + 服务段(按 Name 取)
→ obs.Init（日志先行）
→ 按配置初始化 db/redis/memcache/kafka（配了才建，未配 nil）
→ svc.Setup(ctx, env)
→ httpx.New（/healthz /readyz /metrics）+ svc.BuildRouter
→ 运行（信号 → 优雅退出）；配置文件 watch → ConfigWatcher 回调
```

关键点：**"配了才建，没配为 nil"**——这是脚手架友好度的核心。一个只有 `http:` 段的最小配置也能起服务，其余依赖随加随用。

## 4. 依赖注入的风格

不引入依赖注入框架，用**显式 Deps 结构**：

```go
s.hello = helloworld.New(helloworld.Deps{DB: env.DB, Redis: env.Redis, ...})
```

理由：编译期类型安全、无魔法、AI 易读易生成。代价是装配代码手写——但装配点集中在 `internal/app` 一处，可接受。

## 5. 一致性由工具保证（而非纪律）

- `.golangci.yml` depguard：业务不 import 驱动库（单一基础设施入口），不 import Web 框架（单一 HTTP 入口）。
- `make ci`：vet + test-short + build；`make check` 预留生成物校验位。

## 6. 演进方向（预留、未做）

- OpenTelemetry trace / metrics 导出（obs 层已隔离）。
- 熔断/限流中间件（httpx 中间件链可插拔）。
- 配置中心接入（config.Source 抽象点已预留）。
- 多服务模板（当前单服务 `cmd/demo`，复制改名即可）。
