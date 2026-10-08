---
name: Matex Setup Cron
description: 在 matex 中加定时任务，多副本下用 redis 选主保证每个周期只跑一次。当用户说"定时任务 / 定时器 / cron / 周期任务 / 后台任务 / 跑批 / 选主 / leader election / 只能跑一次 / schedule"时使用。
---

# 接入定时任务

用 `pkg/core/cron`。它解决的问题是**多副本部署下每个周期只跑一次**，而不是每副本各一次。

## 步骤

### 1. 定义 job 并在 Setup 里组装

```go
func (s *Svc) Setup(ctx context.Context, env *verticle.Env) error {
	sched := cron.New(cron.Config{
		Redis:   env.Redis,     // nil = 单副本，不做选主
		Timeout: time.Minute,   // 单次运行上限
	})
	sched.Add(
		cron.Job{Name: "expire-orders", Interval: 5 * time.Minute, Run: s.expireOrders},
		cron.Job{Name: "daily-report", Interval: 24 * time.Hour, RunOnStart: true, Run: s.dailyReport},
	)

	// 关键：用 env.Block 挂上去，verticle 会在关掉基础设施之前先停 job
	env.Block("cron", sched.Run)
	return nil
}
```

`Run(ctx) error` 阻塞到 ctx 取消，然后等正在跑的 job 返回。

### 2. 配置（可选）

配了 `redis:` 段就自动多副本选主；没有就是单副本直接跑：

```yaml
redis:
  addr: ${REDIS_ADDR:localhost:6379}
```

## 必须记住的三条

**1. claim 持有整个 `Interval`，所以 `Interval` 也是 run 的时间上限。**

各副本 ticker 不同步。若 claim 在 job 跑完就释放，另一个副本会在自己的下一个 tick 上抢到
空的 claim，再跑一遍 —— 每个周期执行 N 次，选主失效。让 claim 自然过期（语义是"这个周期
已被占用"）才能保证每周期一次。

因此：**job 必须远快于 interval**。跑超时的 job（interval 5s 却跑 8s）会让别的副本提前
接手。用 `Config.Timeout` 兜底。

**2. `Name` 就是锁 key。**

- 多个副本必须用**同一个** job 名（这正是选主的前提）；
- 不同 job 必须用**不同**名字，否则会互相吞掉对方的周期。

**3. 用 `env.Block` 注册，不要自己 `go sched.Run(ctx)`。**

`verticle` 保证关闭顺序：先停 job，再关 DB/redis —— 反过来的话 job 会在已经关掉的连接上跑。

## 其它行为

| 情况 | 行为 |
|---|---|
| job 返回 error | 记 error 日志，**下一轮照常跑** |
| redis 不可用 | **跳过这个周期**（宁可漏一轮，也不让每个副本都跑） |
| 参数非法 | `Add` 时 **panic** —— 配错的定时任务该在启动时暴露 |
| `RunOnStart: true` | 启动立刻跑一次，不等第一个 interval |

## 验证

```sh
go test ./pkg/core/cron ./examples/cron
make ci
```

测选主时别调未导出的 `runOnce`，用两个真的跑起来的 scheduler 抢一次 claim ——
参考 `examples/cron` 的 `TestLeaderElectionRunsOneInstancePerInterval`。
