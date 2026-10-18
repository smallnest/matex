# audit — 审计日志

`pkg/core/audit` 记录**谁对什么资源做了什么、结果如何**，是合规审查和事故排查读的那条线索。

## 运行

```sh
go run ./examples/audit

curl -i -X POST localhost:8086/api/v1/orders/42/refund -H 'X-API-Key: demo-key'   # 200
curl -i -X POST localhost:8086/api/v1/orders/42/refund -H 'X-API-Key: demo-key'   # 409，同样入审计
curl -s localhost:8086/api/v1/audit                                              # 内存里的审计轨迹

go test ./examples/audit
```

## 用法

```go
audit.Record(ctx, audit.Event{
    Actor:    principal.ID,                  // 用户 id / 服务名 / "system"
    Action:   "order.refund",                // 点分动词形式
    Resource: fmt.Sprintf("order/%d", id),   // 资源标识
    Result:   audit.Success,                 // 或 audit.Failure
    Detail:   map[string]string{"reason": "already refunded"}, // 可选、可 JSON 化
})
```

`Record` 会补上时间（`At`）和当前请求的 trace id（`TraceID`，可直接和日志联动）。

## 输出到哪里

| sink | 方式 | 适用 |
|---|---|---|
| 默认 | 一条结构化日志 `msg="audit"`，字段 `audit_actor/audit_action/audit_resource/audit_result/audit_detail/audit_at` + `trace_id` | 已有日志管道（ELK / Loki） |
| 自定义 | `audit.SetSink(s)`，`s.Write(ctx, Event)` 同步调用 | 审计表、只追加的 Kafka topic、WORM 对象存储 |

示例把审计事件同时写进内存表，方便在 `/api/v1/audit` 读回来；真实服务里换成数据库写入即可。

## 约定

- **每个结果都记**：成功和失败（409/403 拒绝）都是审计事实，别只记成功。
- **业务动作之后立即记**，不要攒批、不要异步丢弃。
- **Detail 不放秘密**：不放 token、密码、完整请求体。
- 包只保证**字段完整 + 带时间戳**；顺序、保留期、不可篡改、访问控制由 sink 负责。
- sink 写失败要上报（返回错误或告警），否则审计轨迹会有洞。
