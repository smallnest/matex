# cron — 定时任务与选主（cron）

`pkg/core/cron` 解决的问题：**多副本部署下，一个定时任务每个周期只跑一次**，而不是每个
副本各跑一次。

做法是每个副本都起 ticker，到点时抢一个以 job 命名的 redis 认领（claim），抢到的那个执行。
不配 redis（单副本、测试）就直接跑。

## 运行

```sh
go run ./examples/cron

curl -s localhost:8084/api/v1/jobs     # 每个 job 跑了多少次、最后什么时候跑的
curl -s localhost:8084/version         # 内置的构建信息端点

go test ./examples/cron
```

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
	Redis:   env.Redis,      // nil = 单副本，不做选主
	Timeout: time.Minute,    // 单次运行的上限
})
sched.Add(
	cron.Job{Name: "heartbeat", Interval: time.Second, RunOnStart: true, Run: s.heartbeat},
	cron.Job{Name: "reconcile", Interval: 5 * time.Minute, Run: s.reconcile},
)
env.Block("cron", sched.Run)   // verticle 启动它、退出时取消它
```

用 `env.Block` 挂上去，`verticle` 会在**关闭基础设施之前**先停掉 job —— 否则 job 可能
在数据库已经关掉之后还在跑。

## 关键设计：claim 持有**整个 interval**

这条最容易踩，也是这个包唯一值得记住的实现细节：

认领**不在 job 跑完后释放**，而是让它在 `Interval` 之后自然过期。

原因：各副本的 ticker **不同步**（各自启动时刻不同）。假设 A 在 t=0/10/20… tick，B 在
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

## 几个约定

- **`Name` 是锁 key**：同名即同锁。多个副本必须用同一个名字（这很正常），但**不同 job
  必须用不同名字**，否则会互相吞掉对方的周期。
- **job 失败不会停循环**：示例里 `reconcile` 每第三次返回 error，下一轮照跑。
- **redis 不可用时跳过这个周期**，而不是每个副本都跑 —— 宁可漏一轮，也不要惊群。
- **不用写 `if env.Redis != nil`**：`Config.Redis` 为 nil 就是单副本模式。
- 参数非法（缺 `Name`/`Interval`/`Run`）在 `Add` 时 **panic**：配错的定时任务应该在启动
  时报错，而不是几周后才发现它从没跑过。

## 测试

不需要 Docker：`redistest.Start(t)` 起进程内的 miniredis，`cron` 的 claim 就能工作。

验证选主时**不要**去调未导出的 `runOnce` —— 用两个各自跑起来的 scheduler，让它们真的
抢一次 claim。参考 `examples/cron` 的 `TestLeaderElectionRunsOneInstancePerInterval`。
