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
| 分布式锁 | redis `TryLock`：带 token + Lua 释放 + TTL 兜底；`Claim`：窗口式占领（选主用） |
| HTTP 中间件链 | `httpx.Use`（封套层，可短路返回 `errs`）/ `UseOuter`（net/http 层），横切能力都挂这里 |
| 认证与授权 | `auth`：HMAC/JWKS 验签、`Principal` 进 ctx、`RequireScope/Role`（配 `auth:` 段启用） |
| 链路追踪 | `obs`：OpenTelemetry span + W3C 传播 + OTLP 导出（配 `trace:` 段启用） |
| 稳定性防护 | `ratelimit`（本地令牌桶 / redis 固定窗口）、`breaker`、`retry`（全抖动退避）、`idempotency` |
| 缓存模式 | `cache`：cache-aside + 并发 miss 合并（防击穿）+ TTL 抖动（防雪崩） |
| 定时任务 | `cron`：多副本 redis 选主，claim 持有整个 interval |
| 运维端点 | `/version`（build info）、`/debug/pprof/*`（`http.pprof` 开启） |
| 测试基座 | dbtest（默认纯 Go SQLite + 自动迁移，可切真库）、redistest（miniredis） |
| 一致性守卫 | depguard 两条规则（业务禁驱动库、禁 Web 框架） |
| 代码生成 | **AI + skills**（`skills/` + `AGENTS.md`） |

### 2.2 刻意不做什么（及理由）

| 舍弃 | 理由 |
|---|---|
| ORM | SQL 直写 + `QueryOne[T]` 已够；ORM 增加黑盒与逃逸面。 |
| gin/echo/chi | Go 1.22 ServeMux 原生支持方法路由 + 路径参数，`httpx` 补齐统一响应与观测；再引入第三方路由只会增加依赖面与分叉。 |
| 服务发现 / 注册中心 | K8s 拓扑 + 网关单一入口；静态 DNS + 环境变量足够。 |
| 默认开启的重治理 | 熔断、限流、幂等、链路追踪都**有薄封装，但一律 opt-in**（见 2.3）。脚手架不替使用者决定开启重治理，也不把它们的开销强加给一个只想跑 HTTP 的服务。 |
| 配置热更的设施重建 | 热更只回调服务段配置，不重建 db/redis 连接（避免复杂度爆炸）。 |
| 代码生成器二进制 | 生成器是"会过时的约定"，skill 是"给 AI 的约定"——AI 读 `AGENTS.md` + skill 即可生成符合规范的代码，且模板随仓库演进。 |

### 2.3 提供了、但默认关闭的能力

2.2 反对的是**默认开启的重治理**，不是这些能力本身。它们都在脚手架里，但全部 opt-in：
不配对应的配置段、不在代码里显式挂载，就完全不存在（no-op 或 nil），一个只有 `http:` 段
的服务不会为它们付出任何代价。

| 能力 | 开关 | 为什么值得留在服务内，而不是只靠网关 / K8s |
|---|---|---|
| 限流 `ratelimit` | 显式 `Use` | 网关限流按路由/租户，粒度粗；这里能按用户、API key、业务维度 |
| 熔断 `breaker` / 重试 `retry` | 包住具体调用 | 网关看不到"本服务到某个下游"的失败率；重试本来就是出站的事 |
| 幂等 `idempotency` | `Use` + `Idempotency-Key` | 网关无从判断"这两个请求是同一笔业务" |
| 链路 `obs` tracing | `trace.enabled` | 跨服务因果链，网关只覆盖入口 |
| 缓存模式 `cache` | 调用处 | 防击穿/防雪崩是数据访问层的职责 |
| 定时任务 `cron` | `env.Block` | 网关不跑定时任务；多副本选主要业务语义（claim 持有整个 interval） |
| pprof | `http.pprof` | 排障必需，但 heap 里可能有凭证，故默认关且只该在受信网络暴露 |

共同约定：**防护组件自身故障时放行**（限流器、幂等存储出错都只记 warn 不拒流量）——
降级优先于熔断主流程。

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
→ obs.Init（日志先行）→ obs.InitTracing（配了 trace 段才建 provider）
→ 按配置初始化 db/redis/memcache/kafka/auth（配了才建，未配 nil）
→ svc.Setup(ctx, env)
→ httpx.New（/healthz /readyz /metrics /version，+ http.pprof 时挂 /debug/pprof/）+ svc.BuildRouter
→ 运行（信号 → 优雅退出：先停 Block 里的定时任务，再 flush trace，最后关基础设施）；配置文件 watch → ConfigWatcher 回调
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

- 配置中心接入（config.Source 抽象点已预留）。
- 多服务模板（当前单服务 `cmd/demo`，复制改名即可）。
- gRPC / rpcx 的链路传播（HTTP 侧与出站 `InjectHTTP` 已通，跨进程的 gRPC interceptor 未接）。
- 对象存储、多租户、审计日志、feature flag —— 判断为按需再加，暂不进脚手架。
- 本地多级缓存（需要多实例失效方案，收益未经压测验证，故 `cache` 只做单层 redis）。
