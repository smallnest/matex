# auth — 认证与授权

`pkg/core/auth` 把 Bearer token 验签落到 `httpx` 的中间件插槽上。**handler 不解析
`Authorization`、不手写 401**：中间件验完 token 把 `Principal` 放进 ctx，handler 直接读。

## 运行

```sh
go run ./examples/auth

TOKEN=$(curl -s -X POST localhost:8081/api/v1/login \
  -d '{"username":"bob","password":"bob-pw"}' | sed 's/.*"token":"\([^"]*\)".*/\1/')

curl -i localhost:8081/api/v1/profile                                        # 401，无 token
curl -i localhost:8081/api/v1/profile     -H "Authorization: Bearer $TOKEN"   # 200
curl -i localhost:8081/api/v1/admin/audit -H "Authorization: Bearer $TOKEN"   # 200，bob 是 admin
# 换 alice 的 token 调 /admin/audit → 403，她没有 admin 角色

go test ./examples/auth
```

## 两种验签模式（配置二选一）

| 配置字段 | 算法 | 用途 |
|---|---|---|
| `auth.secret` | HS256/384/512 | 内部调用、测试 |
| `auth.jwks_url` | RS256/RS384/RS512/ES256/… | 对接 OIDC，公钥从 JWKS 端点拉取 |

```yaml
auth:
  issuer: https://idp.example.com                            # 可选：校验 iss
  audience: orders-api                                       # 可选：校验 aud
  jwks_url: https://idp.example.com/.well-known/jwks.json    # 或 secret: ${AUTH_SECRET}
  leeway: 30s                                                # 容忍时钟偏移
  jwks_ttl: 10m                                              # JWKS 缓存时长
```

同时配 `secret` 和 `jwks_url` 会**启动失败**（`auth.New` 报错）——不给出模棱两可的
验签路径。

## 授权：靠 `Use` 的累积语义表达层级

`Use` 只护它之后注册的路由，所以**公开 → 登录态 → 更高权限**就是三行：

```go
srv.Handle("POST", "/api/v1/login", s.login)   // 公开

srv.Use(s.verifier.Middleware())               // 这一行往下都要 token
srv.Handle("GET", "/api/v1/profile", profile)

srv.Use(auth.RequireRole("admin"))             // 再加一层：要 token + admin
srv.Handle("GET", "/api/v1/admin/audit", audit)
```

三个开箱守卫：`RequireScope(scopes...)`、`RequireRole(roles...)`、`RequireAnyRole(roles...)`。
细粒度（资源级）授权在 handler 里用 `p.HasScope(...)` 自己判，需要时返回
`errs.Forbidden(40301, …)`。

## handler 里读主体

```go
p, ok := auth.FromContext(ctx)
if !ok {
    return nil, errs.Unauthorized(40101, "no authenticated principal")
}
return orders.List(ctx, p.Subject)
```

`Principal` 暴露 `Subject`、`Issuer`、`Scopes`、`Roles`、`Claims`（原始 claim 集）。

## 失败语义：基础设施故障不是凭证问题

| 情况 | 响应 |
|---|---|
| 缺 token / 格式错 / 签名错 / 过期 | `401 {"code":40101}` |
| 有 token 但 scope/role 不够 | `403 {"code":40301}` |
| **JWKS 端点不可达** | `503 {"code":50301}` |

最后一条是刻意的：身份提供方短暂不可用不该让客户端以为自己的 token 坏了。JWKS 拉取
失败时会**继续用上一次的公钥**（可用性优先），只有从未成功拉取过才对外报 503。

## 其它能力

- **`Optional()`**：有 token 就解析、没有就放行（公开页面登录后个性化）。注意*畸形*
  token 仍然拒绝——静默忽略坏凭证只会藏 bug。
- **密钥轮转**：JWKS 里出现没见过的 `kid` 会立即重取一次文档，不用等 `jwks_ttl` 过期。
- **`Issue(IssueRequest{...}, ttl)`**：HMAC 模式下签发 token，服务内部/测试不必自己
  import JWT 库。`Extra` 不能覆盖 `sub/iat/exp/iss/aud`。
- 只接受非对称算法的 JWKS 模式**拒绝** HS256 token —— 防经典的算法混淆攻击。

## 约定

- 守卫中间件**失败关闭**：没有认证中间件在前，`RequireScope` 会 401 而不是放行。
- 本包依赖 `golang-jwt/jwt/v5`（零传递依赖）。JWT 验签是安全敏感逻辑，不自研。
