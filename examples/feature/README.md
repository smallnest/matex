# feature — 功能开关

`pkg/core/feature` 是进程内的功能开关：按名字读一个布尔值，按部署环境切换代码路径，
不用改代码、不用重新编译。

## 运行

```sh
go run ./examples/feature

curl -s localhost:8085/api/v1/checkout                              # flag 决定走 v1 还是 v2
curl -s -X POST "localhost:8085/api/v1/flags/beta-search?on=true"   # 运行期翻转
curl -s localhost:8085/api/v1/flags                                 # 所有开关的当前值

FEATURE_BETA_SEARCH=on go run ./examples/feature                    # 环境变量优先于配置
go test ./examples/feature
```

## 用法

```go
// 声明（init 或 Setup 里），Register 只提供默认值
var newCheckout = feature.Flag{Name: "new-checkout", Default: false}
func init() { feature.Register(newCheckout) }

// 读取：每次请求都读，翻转后下一次请求立即生效
if newCheckout.Enabled() {
	return s.checkoutV2(ctx, in)
}
return s.checkoutV1(ctx, in)
```

`configs/config.yaml` 里的 `feature:` 段（`verticle.Run` 启动时自动灌入）：

```yaml
feature:
  new-checkout: true
  beta-search: false
```

## 取值优先级（高 → 低）

| 来源 | 写法 | 适用 |
|---|---|---|
| 运行时 | `feature.Set("beta-search", true)` | 管理接口 / 控制面板即时开关 |
| 环境变量 | `FEATURE_BETA_SEARCH=on`（非字母数字变 `_`，大写） | 每个部署环境单独覆盖 |
| 配置 | `feature: {beta-search: true}` | 随配置一起发布 |
| 默认值 | `feature.Register(feature.Flag{Default: …})` | 没被覆盖时的值 |

- 环境变量接受 `1/true/on/yes` 与 `0/false/off/no`；无法解析的值会被忽略（回落到下一层）。
- 没注册过的开关默认关闭；也可以在配置/环境里直接给新名字开开关。
- 开关只作用于**本进程**：多副本要统一时，由控制面板对每个副本调 `Set`，或走发布配置。

## 测试

测试里用 `feature.Configure(map[string]bool{...})` 隔离取值（它会整体替换配置层）；
环境变量用 `t.Setenv`。
