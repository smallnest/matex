---
name: Matex Setup Auth
description: 在 matex 中接入认证与授权：配置 auth 段、验签 Bearer token、按路由分级保护、读取当前主体。当用户说"加认证 / 加鉴权 / 接 auth / setup auth / JWT / OIDC / 登录校验 / token 校验 / 权限控制 / RBAC"时使用。
---

# 接入认证与授权

认证经 `pkg/core/auth` 使用（Bearer token 验签 + `Principal` + 三个守卫中间件），
业务不 import JWT 库，handler 不解析 `Authorization`、不手写 401。

## 步骤

### 1. 配置（configs/config.yaml）

两种模式**二选一**（同时配会启动失败）：

```yaml
auth:
  # 模式 A：共享密钥（内部调用、测试）
  secret: ${AUTH_SECRET}

  # 模式 B：对接 OIDC，公钥从 JWKS 端点拉
  # jwks_url: https://idp.example.com/.well-known/jwks.json

  issuer: https://idp.example.com     # 可选：校验 iss
  audience: orders-api                # 可选：校验 aud
  leeway: 30s                         # 容忍时钟偏移
  jwks_ttl: 10m                       # JWKS 缓存时长
```

留空/删除 `auth:` 段即禁用（`env.Auth` 为 nil）。`verticle` 在配了 `secret` 或
`jwks_url` 时构建 `env.Auth`。

### 2. 在 Setup 里取 verifier

```go
func (s *Svc) Setup(_ context.Context, env *verticle.Env) error {
	if env.Auth == nil {
		return errors.New("svc: the `auth` section must be configured")
	}
	s.verifier = env.Auth
	return nil
}
```

### 3. 在 BuildRouter 里分级保护

`Use` 只护它之后注册的路由，所以层级就是书写顺序：

```go
func (s *Svc) BuildRouter(srv *httpx.Server) error {
	srv.Handle("POST", "/api/v1/login", s.login)   // 公开

	srv.Use(s.verifier.Middleware())               // 这一行往下都要 token
	srv.Handle("GET", "/api/v1/profile", profile)

	srv.Use(auth.RequireRole("admin"))             // 再加一层：token + admin
	srv.Handle("GET", "/api/v1/admin/audit", audit)
	return nil
}
```

守卫：`auth.RequireScope("orders:read")`、`auth.RequireRole("admin")`、
`auth.RequireAnyRole("ops", "sre")`。

### 4. handler 里读主体

```go
func listOrders(ctx context.Context, r *http.Request) (any, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, errs.Unauthorized(40101, "no authenticated principal")
	}
	// 细粒度（资源级）授权自己判
	if !p.HasScope("orders:read") {
		return nil, errs.Forbidden(40301, "missing scope orders:read")
	}
	return orders.List(ctx, p.Subject)
}
```

`Principal` 暴露 `Subject` / `Issuer` / `Scopes` / `Roles` / `Claims`。

### 5. 签发 token（仅 HMAC 模式）

```go
token, err := s.verifier.Issue(auth.IssueRequest{
	Subject: user.ID,
	Scopes:  user.Scopes,
	Roles:   user.Roles,
}, time.Hour)
```

`Extra` 不能覆盖 `sub/iat/exp/iss/aud`。非对称模式下签发在 IdP 侧，本方法会报错。

## 约定（必须遵守）

- **失败关闭**：守卫中间件要求前有认证中间件；没有 principal 时返回 401，不放行。
- 登录失败时"用户不存在"和"密码错误"返回**同一条消息**，避免用户名枚举。
- 公开路由必须先注册——`Use` 不回头保护已注册的路由。
- JWKS 端点不可达返回 `503` 而非 `401`（见下）。
- 生产环境 `secret` 走 `${AUTH_SECRET}` 环境变量注入，不落配置文件。

## 错误语义

| 情况 | 响应 |
|---|---|
| 缺 token / 格式错 / 签名错 / 过期 | `401 {"code":40101}` |
| scope / role 不足 | `403 {"code":40301}` |
| JWKS 端点不可达 | `503 {"code":50301}` |

## 测试（无 Docker）

HMAC 模式完全离线：`auth.New(Config{Secret: "..."})` + `Issue(...)` 造 token，
用 `httptest` 打 `srv.Handler()`。JWKS 模式用 `httptest.NewServer` 假一个 JWKS 端点。
参考 `examples/auth`。

## 验证

```sh
go test ./pkg/core/auth ./examples/auth
make ci
```
