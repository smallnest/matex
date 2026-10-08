# kafka — 生产者与消费者

`pkg/core/kafka` 包住 franz-go：`NewProducer` 默认 `acks=all`（durable），
`RunConsumer` 是 at-least-once 消费循环（消费完**同步提交**，关停时用 detached
context 提交最后一批，避免重复投递给下一个成员）。

## 运行

```sh
make dev                                  # 起 Kafka（KRaft 单节点）
go run ./examples/kafka

curl -i -X POST localhost:8080/api/v1/events       -d '{"key":"u1","value":"hello"}'
curl -i -X POST localhost:8080/api/v1/events/async -d '{"key":"u2","value":"fire-and-forget"}'
curl -i localhost:8080/api/v1/stats

go test ./examples/kafka                  # 用 kfake（进程内 broker），离线可跑
```

## 生产者

```go
p, err := kafka.NewProducer(kafka.Config{Brokers: []string{"localhost:9092"}, ClientID: "svc"})

err = p.SendSync(ctx, topic, key, value)   // 等 broker 确认；不能丢就用它
p.Send(ctx, topic, key, value)             // fire-and-forget；失败只记日志
err = p.SendJSON(ctx, topic, key, obj)     // 同步 + JSON
```

`verticle.Run` 按 `kafka:` 段创建**一个** `env.Kafka` 生产者给你复用
（`if env.Kafka != nil`），不需要自己 new，也不需要自己 Close。

## 消费者

```go
env.Block("consumer", func(ctx context.Context) error {
    return kafka.RunConsumer(ctx, env.KafkaCfg, group, []string{topic},
        func(ctx context.Context, m *kafka.Message) error {
            // 返回 error 只记日志；本批照样提交（at-least-once）
            return handle(ctx, m)
        })
})
```

- `env.Block` 让它随应用启动、随 ctx 取消停止 —— 不需要自己管 goroutine。
- 组名（`group`）决定 offset 提交位置；换组名会**从头消费**。
- 起始位点是 `AtStart`（`ConsumeResetOffset`）：新组会把 topic 里的历史消息全部读一遍。
- 幂等要自己做：at-least-once 意味着**可能重复**。用 key 去重或写库用唯一约束。

## 何时不用 kafka

只需要进程内解耦的话，一个带缓冲的 channel 就够了。引入 kafka 的门槛是：
需要**跨实例、可回放、有顺序保证（单 partition 内）**的事件流。
