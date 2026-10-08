# errors — 错误模型

`pkg/core/errs` 是 matex 的错误模型：一个错误 = **Kind**（映射 HTTP 状态）+ **业务 code** +
**消息**。HTTP 层统一渲染成 `{"code":…,"msg":…,"data":null}`。

## 运行

```sh
go run ./examples/errors
go test ./examples/errors
```

## 关键点

- **Kind → 状态**：`KindNotFound → 404`、`KindConflict → 409`、`KindTimeout → 504`……
- **编码约定**：`<http-status>*100 + 子号`，例如 `40401` = 404 下的"用户不存在"。
  业务 code 是每服务自己的常量。
- **`errors.As` 可穿透**：用 `fmt.Errorf("…: %w", err)` 或 `errs.Wrap` 包装后，
  `errs.Status` 仍能取到 Kind/code。
- **cause 只进日志**：`errs.Wrap(kind, code, cause, msg)` 的 `Error()` 含 cause，
  但响应体只回 `msg` —— 数据库报错、内部细节不会泄露给客户端。
- **兜底**：非 matex 错误统一 `500 / 50000 / "internal error"`。

## 在 handler 里怎么用

```go
func (h *Handler) Get(ctx context.Context, r *http.Request) (any, error) {
	id := r.PathValue("id")
	if id == "" {
		return nil, errs.Invalid(40001, "id is required")   // → 400
	}
	u, err := h.svc.Get(ctx, id)
	if errors.Is(err, db.ErrNoRow) {
		return nil, errs.NotFound(40401, "user %s not found", id) // → 404
	}
	if err != nil {
		return nil, err                                // 交给框架 → 500
	}
	return u, nil
}
```
