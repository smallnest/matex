# matex 示例

`pkg/core/*` 的每一项能力配一个**可运行**示例。除 redis / memcache / kafka 需要真实中间件
（`make dev` 起）外，其余都能**离线运行**：不需要 Docker、不需要网络。

| 示例 | 展示的能力 | 外部依赖 | 运行 |
|---|---|---|---|
| [config](config/) | 配置加载：tag（`default`/`env`/`optional`）、`${VAR}` 展开、环境变量覆盖 | 无 | `go run ./examples/config` |
| [errors](errors/) | 错误模型 `errs`：Kind → HTTP 状态、业务 code、Wrap 不泄露细节 | 无 | `go run ./examples/errors` |
| [database](database/) | `db`：三驱动、`?` 占位符、`QueryAll/One/Scalar` 泛型扫描、`WithTx`、`Querier`（DAO 进出事务） | 默认纯 Go SQLite | `go run ./examples/database` |
| [cache](cache/) | `cache`：cache-aside、并发 miss 合并（防击穿）、TTL 抖动（防雪崩）、写后失效 | Redis | `make dev && go run ./examples/cache` |
| [http](http/) | `httpx`：路由、handler 约定、响应封套、错误映射、`HandleRaw`、中间件槽 | 无 | `go run ./examples/http` |
| [auth](auth/) | `auth`：Bearer token 验签（HMAC/JWKS）、分级授权、`Principal` 进 ctx | 无 | `go run ./examples/auth` |
| [resilience](resilience/) | `ratelimit`/`idempotency`/`breaker`/`retry`：限流、幂等、熔断、退避重试 | 无 | `go run ./examples/resilience` |
| [observability](observability/) | `obs`：结构化日志、相关 id（trace id / X-Request-ID）、Prometheus 指标、OpenTelemetry 链路 | 无 | `go run ./examples/observability` |
| [lifecycle](lifecycle/) | `verticle`/`app`：`Block`、`Closer`、配置热更、优雅退出 | 无 | `go run ./examples/lifecycle` |
| [cron](cron/) | `cron`：定时任务，多副本下 redis 选主保证每周期只跑一次 | 无（选主需 Redis） | `go run ./examples/cron` |
| [redis](redis/) | `redis`：缓存读写、`TryLock` 分布式锁 | Redis | `make dev && go run ./examples/redis` |
| [memcache](memcache/) | `memcache`：JSON 缓存、TTL、`Touch` | Memcached | `make dev && go run ./examples/memcache` |
| [kafka](kafka/) | `kafka`：生产者（同步/异步）+ 消费者循环 | Kafka | `make dev && go run ./examples/kafka` |
| [grpc](grpc/) | `grpcx`：服务注册（`GRPCRegistrar`）+ 客户端拨号（`Env.GRPCClient`） | 无 | `go run ./examples/grpc` |
| [rpcx](rpcx/) | `rpcx`：无 IDL 服务注册（`RPCXRegistrar`）+ 客户端调用 | 无 | `go run ./examples/rpcx` |

也可以 `make example NAME=http`。

## 离线验证

示例的核心逻辑都写了测试，**不依赖任何外部服务**（SQLite / miniredis / kfake / 进程内
gRPC·rpcx 服务）：

```sh
go test ./examples/...
```

## 约定

示例和业务代码同一套规矩：**只经 `pkg/core/*` 访问基础设施，不直接 import 驱动库**
（`depguard` 强制；见 `.golangci.yml` 的 `examples-no-drivers`）。

每个示例的目录里都有一份自己的 `config.yaml`，`main.go` 的 `-conf` 默认指向它，
所以从仓库根目录 `go run ./examples/<name>` 就能跑。
