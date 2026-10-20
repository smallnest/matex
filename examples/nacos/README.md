# nacos — 配置中心

`pkg/core/nacos` 把 Nacos 配置中心接到 `verticle` 上：**本地文件是底座，Nacos 覆盖它有的键**。

- 框架段（`http` / `log` / `db` / `redis` …）来自 `data_id`（默认 `matex.yaml`）；
- 服务段（`Service.Name()` 那段，这里是 `demo`）来自 `service_data_id`（这里是 `demo.yaml`）；
- 服务段变化**实时生效**（走 `ConfigWatcher`，和改本地文件一样）；框架段变化只打日志，
  需要重启或滚动发布——连接池、监听端口、tracing 都是启动时建好的。

## 运行

需要一个 Nacos 服务端（`make dev` 不含它，官方镜像即可）。先在控制台建：

| 项 | 值 |
|---|---|
| namespace | `demo`（填的是 **namespace id**，不是显示名） |
| group | `DEMO` |
| data_id | `matex.yaml`（yaml）：框架段，例如 `http: {addr: ":8080"}`、`log: {level: info}` |
| data_id | `demo.yaml`（yaml）：服务段，例如 `greeting: "Hello from Nacos, "` |

```sh
go run ./examples/nacos
curl -i localhost:8080/api/v1/greeting/world
# 在控制台把 demo.yaml 改成 "Hi from Nacos, " → 保存
# 日志出现 "config reloaded from the center"，接口立刻返回新文案，version +1
```

## 配置（configs/config.yaml）

```yaml
nacos:
  server_addr: ${NACOS_ADDR:localhost:8848}   # 逗号分隔多个地址，可带 https://
  namespace: demo                              # namespace id
  group: DEMO
  data_id: matex.yaml                          # 框架段
  service_data_id: demo.yaml                   # 服务段；删掉则服务段继续读本地文件
  type: yaml                                   # yaml | properties | text
  timeout: 10s                                 # 首次拉取预算
  username: ${NACOS_USERNAME:nacos}            # 二选一：账号密码
  password: ${NACOS_PASSWORD:nacos}
  # access_key: ${NACOS_ACCESS_KEY:}           # 或：阿里云 RAM AK/SK
  # secret_key: ${NACOS_SECRET_KEY:}
  # sts_token: ${NACOS_STS_TOKEN:}             # 临时凭证时再加
```

- `nacos:` 段**只能放在本地文件里**：它决定去哪里找配置中心，配置中心自己说不出来。
- 账号密码与 AK/SK **二选一**，同时配置会在启动时报错；STS 临时凭证在 AK/SK 之外再加 `sts_token`。
- `namespace` / `group` 参与每一次请求：同一个 Nacos 上按平台、团队、环境隔离就靠它们。

## 启动与热更

```
读本地文件 → 嵌套 `nacos:` 段则连配置中心 → 首次拉取（超时 10s，失败即退出）
  → 框架段合并（本地为底，远端覆盖；嵌套 map 逐键合并）
  → 服务段：有 service_data_id 用它，否则用本地文件
  → 后续变化：服务段 → ConfigWatcher；框架段 → 日志提醒重启
```

几个刻意的选择：

- **数据不存在 = 启动失败**。Nacos 对不存在的 data_id 返回空内容，如果当成"空配置"，
  服务会带着缺省值悄悄跑起来——比直接退出更难排查。
- **首次拉取必须成功**：不会用本地缓存快照顶着跑。
- **远端 map 逐键合并**：远端只写 `redis: {addr: ...}` 时，本地 `redis.password` 仍保留。
- **重载按整体代际**：任一 data_id 变化，重新拉取两个 data_id 再一起交付，不会出现新旧混搭。

## 示例代码

`main.go` 里只有业务：`Setup` 用 `env.DecodeService` 读服务段，`OnServiceConfigChange`
热更并拒绝未知键（否则拼错的键会被解码成零值，服务带着空白配置继续跑）。
`main_test.go` **不需要 Nacos**：用假的 `ConfigSource` 起一次 `verticle.Run`，
验证"启动读远端、推送后实时生效"。

## 不开配置中心时

不配 `nacos:` 段、也不传 `verticle.WithConfig(...)`，进程根本不连 Nacos，行为与以前完全一样。
Nacos 客户端是"显式开启"的能力，不是默认依赖路径。
