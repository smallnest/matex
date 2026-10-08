# http — Web 层（httpx）

零 Web 框架：标准库 `net/http` + Go 1.22 `ServeMux`（方法路由 + `{path}` 参数），
`pkg/core/httpx` 补齐统一响应、超时、trace id、日志、metrics。

## 运行

```sh
go run ./examples/http        # = go run ./examples/http -conf examples/http/config.yaml

curl -i localhost:8080/api/v1/echo/world
curl -i localhost:8080/api/v1/nothing
curl -i localhost:8080/api/v1/errors/notfound
curl -i -X POST localhost:8080/api/v1/users -d '{"name":"alice"}'                            # → 401，缺 key
curl -i -X POST localhost:8080/api/v1/users -H 'X-API-Key: demo-key' -d '{"name":"alice"}'
curl -i localhost:8080/api/v1/admin/whoami -H 'X-API-Key: demo-key' -H 'X-Tag: alice'
curl -i localhost:8080/raw/plain
curl -i -H 'X-Request-ID: trace-abc' localhost:8080/api/v1/echo/x

go test ./examples/http
```

## handler 约定

```go
type HandlerFunc func(ctx context.Context, r *http.Request) (any, error)
```

**handler 不碰 `ResponseWriter`**，也不决定状态码——只管解析入参、调业务、返回数据或错误。

| 返回 | 响应 |
|---|---|
| `data, nil` | `200 {"code":0,"msg":"ok","data":…}` |
| `nil, nil` | `204`（无 body） |
| `nil, errs.NotFound(40401, …)` | `404 {"code":40401,"msg":"…","data":null}` |
| `nil, errors.New("…")` | `500 {"code":50000,"msg":"internal error"}`（不泄露细节） |

框架统一处理的还有：**panic → 500**、**超时 → 504/50400**、**X-Request-ID**（透传或生成，
写回响应头并进日志）、访问日志、`http_server_*` 指标、`max_body` 限流。

## 路由

```go
srv.Handle("GET", "/api/v1/hello/{name}", h.Greet)   // r.PathValue("name")
srv.Handle("POST", "/api/v1/users", h.Create)
```

`Handle` 是 `METHOD /path` 形式（ServeMux 语法）。**重复注册会 panic** —— 路由冲突就该
在启动时暴露，而不是运行期随机命中。

## 中间件

两个插槽，各管一层：

| 插槽 | 签名 | 位置 | 典型用途 |
|---|---|---|---|
| `Use(mw...)` | `func(next HandlerFunc) HandlerFunc` | 框架 wrapper **内部** | 认证、限流、幂等、ctx 注入 |
| `UseOuter(mw...)` | `func(next http.Handler) http.Handler` | mux **外部** | CORS、gzip、RealIP、通用响应头 |

**`Use` 走 envelope**：中间件拿到的是已带 trace id 和超时的 ctx，要拒绝就直接返回 `errs` 值，
**不用碰 `ResponseWriter`**，状态码由框架映射：

```go
srv.Use(func(next httpx.HandlerFunc) httpx.HandlerFunc {
    return func(ctx context.Context, r *http.Request) (any, error) {
        if r.Header.Get("X-API-Key") != key {
            return nil, errs.Unauthorized(40101, "missing X-API-Key")   // → 401 {"code":40101,…}
        }
        return next(ctx, r)
    }
})
```

**`Use` 只护它之后注册的路由** —— 公开/受保护分区靠书写顺序表达：

```go
srv.Handle("POST", "/api/v1/login", login)      // 公开
srv.Use(authMiddleware, rateLimitMiddleware)    // 从这行往下都过中间件
srv.Handle("GET", "/api/v1/orders", listOrders) // 受保护
```

顺序即书写顺序：**先注册的在最外层**（先执行，最后收尾）。中间件可以往 ctx 里塞东西
（当前主体、租户、trace），下游 handler 直接读 —— 见示例里的 `withTag` / `whoami`。

**`UseOuter` 覆盖整个 mux**，所以 `/healthz`、`/readyz`、`/metrics` 和 `HandleRaw` 路由都在
它范围内（这些 `Use` 不管）。它在最外层 `guard`（panic 恢复 + `max_body`）的内侧，所以
中间件自己 panic 也会被兜成 500。

```sh
curl -i -X POST localhost:8080/api/v1/users -d '{"name":"alice"}'                            # 401
curl -i -X POST localhost:8080/api/v1/users -H 'X-API-Key: demo-key' -d '{"name":"alice"}'   # 200
curl -i localhost:8080/api/v1/admin/whoami -H 'X-API-Key: demo-key' -H 'X-Tag: alice'        # ctx 里的 tag
curl -i localhost:8080/healthz                                                               # 也带 X-Served-By
```

## 需要特殊状态码 / 流式响应？

用 `HandleRaw`（原样交给 `net/http`）：

```go
srv.HandleRaw("GET /raw/plain", func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)   // 201/202/206/SSE/文件下载都走这里
	_, _ = w.Write([]byte("ok\n"))
})
```

## 测试

`httpx.Server.Handler()` 返回底层 `http.Handler`，可以直接用 `httptest` 打真实路由
（跑完整 wrapper，但不占端口）：

```go
srv := httpx.New(httpx.Config{Timeout: time.Second})
_ = svc.BuildRouter(srv)
rec := httptest.NewRecorder()
srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/echo/world", nil))
```

## 内置端点

| 路径 | 说明 |
|---|---|
| `GET /healthz` | 存活探针（不查依赖） |
| `GET /readyz` | 就绪探针（ping 已配置的 infra + 服务的 `ReadyChecker`） |
| `GET /metrics` | Prometheus（`verticle` 注入 metrics 后自动注册） |
| `GET /version` | 构建信息（service / go 版本 / 模块 / vcs revision），由 `verticle` 注册 |
| `GET /debug/pprof/*` | 标准库 profiler，**默认关闭**，见下 |

`/version` 在 `BuildRouter` 之前注册，所以不受服务自己加的中间件影响 —— 排查时就该能直接
打到它，不用先去搞一个 token。

### pprof（默认关闭）

```yaml
http:
  pprof: true      # 挂载 /debug/pprof/
```

**只在受信网络开**：heap profile 可能包含凭证，生成 profile 本身也吃 CPU。生产上应该让
它只在内网/运维端口可达，别和业务流量同一个入口。

```sh
go tool pprof http://localhost:8080/debug/pprof/profile?seconds=30   # CPU
go tool pprof http://localhost:8080/debug/pprof/heap                 # 内存
```

pprof 是 raw handler（不走 JSON 封套），所以只受 `UseOuter` 影响，**不受 `Use` 影响** ——
一个拒绝所有请求的业务中间件不会挡住它。
