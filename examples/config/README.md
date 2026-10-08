# config — 配置

`pkg/core/config`：YAML 文件 + 结构体 tag，除了必要的反射没有别的魔法。

## 运行

```sh
go run ./examples/config

# 环境变量优先于文件：
DB_DSN='postgres://localhost/overridden' go run ./examples/config
```

## tag 语义

| tag | 含义 |
|---|---|
| `default:"..."` | key 缺失（或为 nil）时用它 |
| `env:"VAR"` | 环境变量存在时**覆盖**文件里的值 |
| `optional:""` | key 可以缺失，缺了就留零值 |
| 都没有 | key 缺失即**报错** —— 配置错误要在启动时就炸 |

字段名按 `json` tag 解析，没有就退回小写字段名（key 大小写不敏感）。

```go
type Config struct {
    HTTP struct {
        Addr    string        `json:"addr" default:":8080"`
        Timeout time.Duration `json:"timeout" default:"10s"`
    } `json:"http"`
    DB struct {
        DSN          string `json:"dsn" env:"DB_DSN"`
        MaxIdleConns int    `json:"max_idle_conns" optional:""`
    } `json:"db"`
    Features []string `json:"features" default:"a,b"`
}
```

## 环境变量展开

解析前，文件里的 `${VAR}` 与 `${VAR:default}` 会从环境变量展开：

```yaml
db:
  dsn: ${DB_DSN:postgres://localhost/matex?sslmode=disable}
redis:
  addr: ${REDIS_ADDR:localhost:6379}
```

## 文件布局

```yaml
http:            # 框架段（必填）
  addr: ":8080"
log:             # 框架段（必填）
  level: info
db: …            # 可选：配了才初始化
redis: …
kafka: …
demo:            # 服务专属段，key 必须等于 Service.Name()
  greeting: "Hello, "
```

服务段由 `env.DecodeService(&cfg)` 解码，并且是**热更**的单位：
文件一变，`verticle` 去抖 100ms 后调用 `OnServiceConfigChange(raw)`。

## API

```go
config.Load(path, &cfg)       // 读文件
config.Parse(yamlBytes, &cfg) // 直接解析一段 YAML（测试常用）
config.ParseMap(m, &cfg)      // 从 map 解码（热更用）
```

支持的类型：基本类型、`time.Duration`（`"5s"`，数字视为毫秒）、`[]string`（`"a,b,c"` 或 YAML 列表）、
`[]int`、`map[string]T`、嵌套结构体、指向结构体的指针。
