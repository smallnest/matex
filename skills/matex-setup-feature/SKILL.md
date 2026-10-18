---
name: Matex Setup Feature Flags
description: 在 matex 中用功能开关（feature flag）按部署/运行时切换代码路径：pkg/core/feature 的 Register/Enabled/Set/Configure，config.yaml 的 feature 段与 FEATURE_ 环境变量。当用户说"功能开关 / feature flag / 灰度 / 开关 / 按环境打开 / kill switch / 紧急关闭 / toggles"时使用。
---

# 接入功能开关

用 `pkg/core/feature`。它是**进程内**的开关：按名字读布尔值，不用改代码、不用重新编译。

## 步骤

### 1. 声明开关（默认值在这里）

```go
var newCheckout = feature.Flag{Name: "new-checkout", Default: false}

func init() { feature.Register(newCheckout) }   // 或放在 Setup 里
```

### 2. 在业务里按每次请求读取

```go
func (s *Service) checkout(ctx context.Context, in *In) (*Out, error) {
	if newCheckout.Enabled() {          // 不要缓存到 struct 字段，翻转后要立即生效
		return s.checkoutV2(ctx, in)
	}
	return s.checkoutV1(ctx, in)
}
```

### 3. 配置 / 运行时覆盖

```yaml
feature:
  new-checkout: true
```

```sh
FEATURE_NEW_CHECKOUT=on go run ./cmd/<service>    # 名字大写、非字母数字变 _
```

管理接口里（记得加鉴权）：

```go
feature.Set("new-checkout", false)   // 运行时最高优先级，立即生效
```

## 优先级（高 → 低）

`feature.Set` > 环境变量 `FEATURE_<NAME>` > `feature:` 配置段 > `Register` 的 `Default`。
没注册过的名字默认关闭；环境变量接受 `1/true/on/yes`、`0/false/off/no`。

`verticle.Run` 启动时自动把 `feature:` 段交给 `feature.Configure`，不需要自己接。

## 约定

- **在每个请求处读**，不要在 `Setup` 里算一次存起来 —— 那样 `Set` 只对重启生效。
- 开关名用 kebab-case（`new-checkout`），和配置、环境变量保持可互相推导。
- 一个开关只保护一条路径；清理掉旧路径后把开关和旧分支一起删。
- 开关不等于权限控制：`Set` 接口必须鉴权（结合 `pkg/core/auth`）。
- 开关只作用于本进程，多副本要一致就统一发布配置，或由控制面板对各副本调 `Set`。

## 验证

```sh
go test ./pkg/core/feature ./examples/feature
```

参考 `examples/feature`（配置选择路径、运行时翻转、环境变量覆盖，均有测试）。
