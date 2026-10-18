---
name: Matex Setup Cron
description: 在 matex 中加定时任务：固定周期 / cron 表达式 / 一次性任务，多副本下用 redis 选主保证每个周期（槽）只跑一次。当用户说"定时任务 / 定时器 / cron / cron 表达式 / 周期任务 / 后台任务 / 跑批 / 一次性任务 / one-shot / 延迟执行 / 选主 / leader election / 只能跑一次 / schedule"时使用。
---

# 接入定时任务

用 `pkg/core/cron`。它解决的问题是**多副本部署下每个周期（槽）只跑一次**，而不是每副本
各一次。三种调度形态：`Interval`（固定周期）、`Schedule`（cron 表达式）、`At`/`Delay`
（一次性任务）。

## 步骤

### 1. 定义 job 并在 Setup 里组装

```go
func (s *Svc) Setup(ctx context.Context, env *verticle.Env) error {
	rc, _ := env.Redis()    // 未配 redis 段时 rc 为 nil = 单副本，不做选主
	sched := cron.New(cron.Config{
		Redis:   rc,
		Timeout: time.Minute,   // 单次运行上限
	})
	sched.Add(
		cron.Job{Name: "expire-orders", Interval: 5 * time.Minute, Run: s.expireOrders},
		cron.Job{Name: "daily-report", Interval: 24 * time.Hour, RunOnStart: true, Run: s.dailyReport},
		// cron 表达式（标准 5 段，本地时区；支持 @every 5s / @daily 等描述符）。
		// 坏表达式在 Add 时 panic。
		cron.Job{Name: "hourly-cleanup", Schedule: "0 * * * *", Run: s.cleanup},
		// 一次性任务：启动后 10 分钟跑一次；或 At: time.Time 指定绝对时刻。
		cron.Job{Name: "notify", Delay: 10 * time.Minute, Run: s.notify},
	)

	// 关键：用 env.Block 挂上去，verticle 会在关掉基础设施之前先停 job
	env.Block("cron", sched.Run)
	return nil
}
```

`Run(ctx) error` 阻塞到 ctx 取消，然后等正在跑的 job 返回。每个 job 必须恰好设置一种调度
（`Interval` / `Schedule` / `At` / `Delay`），多设或全没设 `Add` 时 panic。

### 2. 配置（可选）

配了 `redis:` 段就自动多副本选主；没有就是单副本直接跑：

```yaml
redis:
  addr: ${REDIS_ADDR:localhost:6379}
```

## 必须记住的几条

**1. claim 持有整个"时间槽"，run 必须在槽内跑完。**

各副本的定时器不同步。若 claim 在 job 跑完就释放，另一个副本会在自己的下一个 tick 上抢到
空的 claim，再跑一遍 —— 每个周期执行 N 次，选主失效。让 claim 自然过期（语义是"这个周期
已被占用"）才能保证每周期一次。

- interval：claim 的 TTL = `Interval`，所以 **job 必须远快于 interval**（interval 5s 却
  跑 8s，别的副本会提前接手）。用 `Config.Timeout` 兜底。
- cron 表达式：claim 的 TTL = 本 cron 槽的长度（下一次触发减本次触发），每槽只跑一次。
  错过不补跑，和主机 cron 一致。
- **一次性任务相反：claim 永不过期（at-most-once）**。抢到一次就永久占用，跨副本最多执行
  一次；失败也不自动重试（记 error 日志，人工介入）。宁可漏，不可双跑。

**2. `Name` 就是锁 key。**

- 多个副本必须用**同一个** job 名（这正是选主的前提）；
- 不同 job 必须用**不同**名字，否则会互相吞掉对方的周期。

**3. 用 `env.Block` 注册，不要自己 `go sched.Run(ctx)`。**

`verticle` 保证关闭顺序：先停 job，再关 DB/redis —— 反过来的话 job 会在已经关掉的连接上跑。

## 其它行为

| 情况 | 行为 |
|---|---|
| interval / cron job 返回 error | 记 error 日志，**下一轮照常跑** |
| 一次性任务返回 error | 记 error 日志，**不再重试**（claim 已永久占用） |
| redis 不可用 | **跳过这个周期**（宁可漏一轮，也不让每个副本都跑） |
| 参数非法（缺 Name/Run、调度没恰好设一个、坏 cron、`RunOnStart` 配在非 interval 上） | `Add` 时 **panic** |
| `RunOnStart: true` | 启动立刻跑一次，不等第一个 interval（仅 interval job） |

## 验证

```sh
go test ./pkg/core/cron ./examples/cron
make ci
```

测选主时别调未导出的 `runOnce`，用两个真的跑起来的 scheduler 抢一次 claim ——
参考 `examples/cron` 的 `TestLeaderElectionRunsOneInstancePerInterval`。
