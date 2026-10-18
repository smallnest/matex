---
name: Matex Setup Audit Log
description: 在 matex 中记录审计日志：pkg/core/audit 的 Record/Event（actor/action/resource/result/detail），默认写结构化日志，可用 SetSink 落库或投递到只追加的存储。当用户说"审计 / 审计日志 / audit log / 操作记录 / 谁改了什么 / 合规 / 不可否认 / access log"时使用。
---

# 接入审计日志

用 `pkg/core/audit`。它保证每条业务动作都有**完整字段 + 时间戳 + trace id**；存储、
保留期、不可篡改由 sink 负责。

## 步骤

### 1. 在业务动作处记录（成功和失败都记）

```go
audit.Record(ctx, audit.Event{
	Actor:    principal.ID,                  // 用户 id / 服务名 / "system"
	Action:   "order.refund",                // 点分动词
	Resource: fmt.Sprintf("order/%d", id),
	Result:   audit.Success,                 // 或 audit.Failure
	Detail:   map[string]string{"amount": "12.00"},
})
```

拒绝（403）和冲突（409）也是审计事实：判定之后、返回错误之前同样 `audit.Record`，
`Result: audit.Failure` 并在 `Detail` 写原因。

### 2. 选择 sink

- 默认：一条结构化日志 `msg="audit"`，字段 `audit_actor/audit_action/audit_resource/audit_result/audit_detail/audit_at` + `trace_id`，沿用已有的日志管道。
- 落库 / 只追加存储：在 `Setup` 里 `audit.SetSink(mySink)`；`Write(ctx, Event)` 是**同步**调用。
- `audit.SetSink(nil)` 恢复默认日志 sink（测试用）。

```go
type dbSink struct{ db *db.DB }
func (s dbSink) Write(ctx context.Context, e audit.Event) {
	// INSERT INTO audit_log(...)；写失败要上报（返回错误/告警），不能吞掉
}
```

## 约定

- **动作完成后立即写**，不要攒批、不要放进会丢事件的异步队列。
- **Detail 不放秘密**：不放 token、密码、完整请求体；只放定位所需的字段。
- `Action` 用稳定的点分动词（改名会让历史查询断档）；`Resource` 用 `类型/id`。
- `TraceID` 由 `Record` 自动从 ctx 取：接上 `obs` 追踪后，审计行能直接跳到对应链路。
- 审计写入失败属于安全隐患：sink 里必须暴露错误，不要静默。

## 验证

```sh
go test ./pkg/core/audit ./examples/audit
```

参考 `examples/audit`（成功与失败都入审计、sink 可插拔、轨迹可读回，均有测试）。
