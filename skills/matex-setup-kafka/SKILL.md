---
name: Matex Setup Kafka
description: 在 matex 中接入 Kafka：配置 kafka 段、生产者发送、消费者注册。当用户说"接 kafka / 配 kafka / setup kafka / 消息队列 / 生产者 / 消费者 / 事件 / 发消息"时使用。
---

# 接入 Kafka

Kafka 经 `pkg/core/kafka` 使用（franz-go 封装），业务不 import franz-go。生产者由 verticle 按配置自动建（`env.Kafka`），消费者在 `Setup` 里用 `env.Block` 注册。

## 步骤

### 1. 配置（configs/config.yaml）

```yaml
kafka:
  brokers: ${KAFKA_BROKERS:localhost:9092}   # 逗号分隔也可
```

留空/删除 `kafka:` 段即禁用（`env.Kafka` 为 nil）。本地 `make dev` 起 kafka（bitnami KRaft 单节点）。消费者的开关属于**服务段**配置（见步骤 3 的 `<name>.consume`），不放 kafka 段。

### 2. 生产者

`Setup` 里 verticle 已经建好 `env.Kafka`，直接注入 domain：

```go
s.foo = foo.New(foo.Deps{Producer: env.Kafka})
```

发送：

```go
// 异步 fire-and-forget：失败只记日志
s.deps.Producer.Send(ctx, "topic", []byte("key"), []byte("value"))

// 同步（不丢消息，用于关键路径）
if err := s.deps.Producer.SendSync(ctx, "topic", []byte("key"), []byte("value")); err != nil {
	return nil, errs.InternalWrap(50001, err, "produce failed")
}

// 发 JSON
if err := s.deps.Producer.SendJSON(ctx, "topic", "key", payload); err != nil { ... }
```

### 3. 消费者（在 internal/app/service.go 的 Setup 里注册）

```go
if s.cfg.Consume {
	env.Block("my-consumer", func(ctx context.Context) error {
		return kafka.RunConsumer(ctx, env.KafkaCfg, "my-group", []string{"topic"}, s.foo.Handle)
	})
}
```

handler 签名：`func(ctx context.Context, m *kafka.Message) error`。返回 error 会 log 并继续（至少一次语义；重试语义在 handler 内实现）。consumer 提交语义：每批 poll 后同步提交，关闭时用 detached context 兜底提交。

## 约定

- topic 名集中常量（如 `helloworld.TopicGreetings`）。
- consumer group 名稳定、按服务命名（`<service>-<用途>`）。
- `Send` 用于可丢事件（日志/通知），关键数据用 `SendSync`。
- 消费者错误：可重试的自处理，不可重试的返回 error 让框架 log 后跳过。

## 验证

```sh
make ci
# 手动：make dev 后
go run ./cmd/demo -conf configs/config.yaml   # demo 段 consume:true 时消费 matex.greetings
```

注意：本地消费验证需要一个 topic。bitnami 镜像可 `docker compose exec kafka kafka-topics.sh --create --topic matex.greetings --bootstrap-server localhost:9092`。
