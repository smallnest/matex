# http — Web 层（httpx）

零 Web 框架：标准库 `net/http` + Go 1.22 `ServeMux`（方法路由 + `{path}` 参数），
`pkg/core/httpx` 补齐统一响应、超时、trace id、日志、metrics。

## 运行

```sh
go run ./examples/http        # = go run ./examples/http -conf examples/http/config.yaml

curl -i localhost:8080/api/v1/echo/world
curl -i localhost:8080/api/v1/nothing
curl -i localhost:8080/api/v1/errors/notfound
curl -i -X POST localhost:8080/api/v1/users -d '{"name":"alice"}'
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
