# cron — 定时任务与选主（cron）

`pkg/core/cron` 解决的问题：**多副本部署下，一个定时任务每个周期只跑一次**，而不是每个
副本各跑一次。支持三种调度形态：

- `Interval`：固定周期（如每 5 分钟）
- `Schedule`：cron 表达式（如 `"0 9 * * *"`，含 `@every 5s` / `@daily` 等描述符）
- `At` / `Delay`：一次性任务（到点跑一次）

做法是每个副本都起自己的定时器，到点时抢一个以 job 命名的 redis 认领（claim），抢到的那个
执行。不配 redis（单副本、测试）就直接跑。

## 运行

```sh
go run ./examples/cron

curl -s localhost:8084/api/v1/jobs     # 每个 job 跑了多少次、最后什么时候跑的
curl -s localhost:8084/version         # 内置的构建信息端点

go test ./examples/cron
```

跑大约 10 秒后：`report` 每 5 秒加 1（cron 表达式 `@every 5s`，见 config.yaml），
`notify` **只出现一次**（一次性任务），`heartbeat` / `reconcile` 按各自的 interval 增长。

看选主效果（需要 `make dev` 起 redis，并打开 config.yaml 里的 `redis:` 段）：

```sh
go run ./examples/cron &       # 副本 A
go run ./examples/cron &       # 副本 B（同一个 job 名 → 同一把 claim）
sleep 2
curl -s localhost:8084/api/v1/jobs   # 计数按 interval 增长，而不是两倍
```

## 用法

```go
sched := cron.New(cron.Config{
	Redis:   rc,            // nil = 单副本，不做选主（rc 来自 env.Redis()）
	Timeout: time.Minute,   // 单次运行的上限
})
sched.Add(
	cron.Job{Name: "heartbeat", Interval: time.Second, RunOnStart: true, Run: s.heartbeat},
	cron.Job{Name: "reconcile", Interval: 5 * time.Minute, Run: s.reconcile},
	// cron 表达式：标准 5 段（本地时区）+ 描述符。坏表达式在 Add 时 panic。
	cron.Job{Name: "daily-report", Schedule: "0 9 * * *", Run: s.dailyReport},
	// 一次性任务：启动后 10 分钟跑一次；或 At: time.Time 指定绝对时刻。
	cron.Job{Name: "notify", Delay: 10 * time.Minute, Run: s.notify},
)
env.Block("cron", sched.Run)   // verticle 启动它、退出时取消它
```

每个 job 必须恰好设置一种调度（`Interval` / `Schedule` / `At` / `Delay`），多设或全没设
在 `Add` 时 panic。`RunOnStart` 只对 `Interval` 有意义。

用 `env.Block` 挂上去，`verticle` 会在**关闭基础设施之前**先停掉 job —— 否则 job 可能
在数据库已经关掉之后还在跑。

## 关键设计：claim 持有**整个时间槽**

这条最容易踩，也是这个包唯一值得记住的实现细节：

认领**不在 job 跑完后释放**，而是让它在槽结束时自然过期。

原因：各副本的定时器 **不同步**（各自启动时刻不同）。假设 A 在 t=0/10/20… tick，B 在
t=5/15/25… tick，interval 都是 10ms。如果 A 在 t=0 拿到锁、跑完就释放，那么 B 在 t=5
时锁已经空了 —— 它会**再跑一遍**。于是每个周期执行两次，选主形同虚设。

让 claim 存活一整个 interval，语义就变成"**这个周期已被占用**"：

| 时刻 | A（t=0,10,20…） | B（t=5,15,25…） |
|---|---|---|
| 0 | 抢到 claim（存活到 10） | — |
| 5 | — | 尝试抢，被占用 → 跳过 |
| 10 | claim 过期，重新抢到 | — |
| 15 | — | 尝试抢，被占用 → 跳过 |

所以 **`Job.Interval` 同时也是 claim 的 TTL**，run 必须远小于 interval。跑超时的 job
（比如 interval 5s、跑了 8s）会让另一个副本在它还没结束时就开始下一轮 —— 用 `Config.Timeout`
给单次运行设上限，并让 interval 明显大于预期耗时。

**cron 表达式同理**：claim 的 TTL 是"这个 cron 槽的长度"（下一次触发减本次触发）。错峰
副本在同一槽内抢不到 claim，所以每槽只跑一次。错过不补跑——一次跑得久，下一次就在当前时刻
之后的下一个触发点，和主机 cron 一致。

**一次性任务相反**：它的 claim **永不过期**（`SetNX` 不带 TTL）。抢到一次就永久占用，
所以跨副本**最多执行一次**——哪怕某次运行失败也不会自动重试（记 error 日志，人工介入）。
宁可漏，不可双跑。单副本（不配 redis）则无条件跑一次。

## 几个约定

- **`Name` 是锁 key**：同名即同锁。多个副本必须用同一个名字（这很正常），但**不同 job
  必须用不同名字**，否则会互相吞掉对方的周期。
- **interval / cron job 失败不会停循环**：示例里 `reconcile` 每第三次返回 error，下一轮照跑。
  一次性任务失败也不会再跑（claim 已永久占用）。
- **redis 不可用时跳过这个周期**，而不是每个副本都跑 —— 宁可漏一轮，也不要惊群。
- **调用点不用判空**：`Setup` 里 `rc, _ := env.Redis()`，`Config.Redis` 为 nil 就是单副本模式。
- 参数非法（缺 `Name`/`Run`、调度没恰好设一个、坏 cron 表达式、`RunOnStart` 配在非
  interval 上）在 `Add` 时 **panic**：配错的定时任务应该在启动时报错，而不是几周后才发现
  它从没跑过。

## 测试

不需要 Docker：`redistest.Start(t)` 起进程内的 miniredis，`cron` 的 claim 就能工作。
`report` 的测试用 `@every 1s`（robfig 的真实时钟最快一秒一档；亚秒会向上取整到一秒）。

验证选主时**不要**去调未导出的 `runOnce` —— 用两个各自跑起来的 scheduler，让它们真的
抢一次 claim。参考 `examples/cron` 的 `TestLeaderElectionRunsOneInstancePerInterval`
与 `TestOneShotLeaderElection`。
