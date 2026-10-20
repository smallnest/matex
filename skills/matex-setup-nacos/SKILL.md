---
name: Matex Setup Nacos
description: 在 matex 中接入 Nacos 配置中心：配 nacos 段、框架段/服务段两个 data_id、namespace/group/鉴权（账号密码或 AK/SK）、服务段热更。当用户说"接 nacos / 配 nacos / setup nacos / 配置中心 / 接入配置中心 / nacos 鉴权"时使用。
---

# 接入 Nacos 配置中心

Nacos 经 `pkg/core/nacos` 使用，由 `verticle.Run` 在加载本地文件之后合并。
业务代码不 import Nacos SDK（depguard 强制）。**本地文件是底座，Nacos 覆盖它有的键**。

## 步骤

### 1. 在文件里配 nacos 段（configs/config.yaml）

```yaml
nacos:
  server_addr: ${NACOS_ADDR:localhost:8848}   # 逗号分隔多个地址，可带 https://
  namespace: ${NACOS_NAMESPACE:}               # namespace id；空 = public
  group: ${NACOS_GROUP:DEFAULT_GROUP}
  data_id: ${NACOS_DATA_ID:matex.yaml}         # 框架段（http/log/db/redis/…）
  service_data_id: ${NACOS_SERVICE_DATA:}      # 服务段（只能填本服务的）；空 = 服务段读本地文件
  type: yaml                                   # yaml | properties | text
  timeout: 10s                                 # 首次拉取预算，超时启动失败
  username: ${NACOS_USERNAME:}                 # 二选一：账号密码
  password: ${NACOS_PASSWORD:}
  access_key: ${NACOS_ACCESS_KEY:}             # 或：阿里云 RAM AK/SK
  secret_key: ${NACOS_SECRET_KEY:}
  sts_token: ${NACOS_STS_TOKEN:}               # 临时凭证时再加
```

规则：

- **namespace 填 id，不填显示名**；namespace 与 group 参与每次请求，是平台/团队/环境隔离的边界。
- `username/password` 与 `access_key/secret_key` **不能同时配**（启动报错）；`access_key` 与 `secret_key`
  必须成对；STS 临时凭证 = AK/SK + `sts_token`。
- `nacos:` 段只能放在**本地文件**里——它决定去哪找配置中心。
- 不配 `nacos:` 段（也不传 `verticle.WithConfig`），进程完全不连 Nacos。

### 2. 在 Nacos 控制台建两个配置

| data_id | 内容 |
|---|---|
| `matex.yaml` | 框架段：`http` / `log` / `db` / `redis` / `kafka` / `feature` … |
| `<service>.yaml` | 只有一个顶层 key：`<service>:`（= `Service.Name()` 小写） |

服务段也可以只写它自己的键（`greeting: hi`），不用再套一层 `<service>:`。

### 3. 代码不写任何 Nacos 相关逻辑

```go
func (s *Service) Setup(ctx context.Context, env *verticle.Env) error {
	var cfg ServiceConfig
	return env.DecodeService(&cfg)   // 合并后的结果，和读文件完全一样
}

func (s *Service) OnServiceConfigChange(raw map[string]any) {
	var cfg ServiceConfig
	if unknown := config.UnknownKeys(raw, &cfg); len(unknown) > 0 {
		obs.Warn(context.Background(), "reload rejected: unknown keys", "keys", unknown)
		return
	}
	if err := config.ParseMap(raw, &cfg); err != nil {
		obs.Warn(context.Background(), "reload rejected", "err", err)
		return
	}
	s.mu.Lock(); s.cfg = cfg; s.mu.Unlock()
}
```

配置中心里的服务段是**平铺的 map**：`ParseMap` 只是把值搬进结构体（字符串仍解码成结构体字段），
没有 tag 级校验。**重载前必须用 `config.UnknownKeys` 拒绝未知键**，否则拼错一个键，服务会带着零值继续跑。
（本地文件热更有 tag 级解码把关，不需要这一步。）

### 4. 自定义数据源（可选）

不想用 nacos 段、或在测试里注入时，传 `verticle.WithConfig(src)`，`src` 实现 `verticle.ConfigSource`：

```go
type ConfigSource interface {
	Framework(body []byte, svcName string) (map[string]any, []string, error)
	Section(body []byte, svcName string) (map[string]any, error)
	Watch(ctx context.Context, onChange func(fw, svc map[string]any)) error
}
```

`Watch` 必须先同步交付第一代配置（调用一次 `onChange`），再在后台推送后续变更。

## 生效规则

| 变更 | 效果 |
|---|---|
| 服务段（service_data_id） | 实时：`env.DecodeService` 换新值 + `ConfigWatcher` 回调 |
| 框架段（data_id） | 只打 `WARN`，需重启/滚动发布（连接池、监听、tracing 都是启动时建的） |
| data_id 不存在 | 启动失败（空内容不当作空配置） |
| 首次拉取失败/超时 | 启动失败（不用本地快照顶着跑） |

## 测试（无 Nacos）

```go
// 用假 source 起一次 Run，验证启动读远端、推送后热更
verticle.Run(ctx, svc, verticle.WithConf(conf), verticle.WithoutConfigWatch(), verticle.WithConfig(fake))
```

`pkg/core/config` 的 `RemoteYAML` / `Extract` / `Merge` 可单测：它们不连网络。

## 验证

```sh
make ci
go run ./examples/nacos    # 需要真实的 Nacos
```
