---
name: Matex Setup Database
description: 在 matex 中接入 SQL 数据库（PostgreSQL / MySQL / SQLite）：配置 db 段、写迁移、写 DAO、跑测试。当用户说"接数据库 / 配 postgres / 配 mysql / 配 sqlite / setup database / 加数据库 / 连数据库 / 写表 / 迁移 / DAO"时使用。
---

# 接入数据库（PostgreSQL / MySQL / SQLite）

数据库经 `pkg/core/db` 统一访问（`database/sql` + 泛型扫描），业务不 import 任何驱动库（depguard 强制）。三种驱动：

| driver | 库 | driver name | DSN 示例 |
| --- | --- | --- | --- |
| `postgres`（默认） | `jackc/pgx/v5/stdlib` | `pgx` | `postgres://user:pass@host:5432/db?sslmode=disable` |
| `mysql` | `go-sql-driver/mysql` | `mysql` | `user:pass@tcp(host:3306)/db?parseTime=true` |
| `sqlite` | `modernc.org/sqlite`（**纯 Go，无需 cgo**） | `sqlite` | `file:matex.db` 或 `:memory:` |

**占位符统一写 `?`**：postgres 下 `pkg/core/db` 会自动改写成 `$1..$n`，所以同一份 SQL 可跨三种库。MySQL 记得加 `parseTime=true`，否则时间列只能扫成字符串。

## 步骤

### 1. 配置（configs/config.yaml）

```yaml
db:
  driver: postgres        # postgres | mysql | sqlite
  dsn: ${DB_DSN:postgres://matex:matex@localhost:5432/matex?sslmode=disable}
  max_open_conns: 16
  max_idle_conns: 2
  conn_max_lifetime: 30m
  connect_timeout: 5s
  slow_threshold: 250ms
```

删掉 `db:` 段即禁用（`env.DB` 为 nil；sqlite 也无需外部服务，可直接 `driver: sqlite` + `dsn: file:matex.db` 跑起来）。

### 2. 写迁移 migrations/NNN_<name>.sql

若只跑一个库，随便写；若要跨库，保持可移植：

```sql
-- 追加不修改；语句以 ; 分隔，不要在一个文件里写 CREATE FUNCTION 体
CREATE TABLE IF NOT EXISTS users (
    id    INTEGER      NOT NULL PRIMARY KEY,   -- mysql 建议 AUTO_INCREMENT，pg 用 BIGSERIAL/GENERATED
    name  VARCHAR(255) NOT NULL,               -- 索引列别用裸 TEXT（MySQL 不能作 PRIMARY KEY）
    score INTEGER      NOT NULL DEFAULT 0
);
```

跨库时的常见坑：

- 自增主键、`ON CONFLICT` / `ON DUPLICATE KEY` / `RETURNING`、`now()` / `CURRENT_TIMESTAMP` 都是各库方言，跨库时避免。
- 需要"取值"就不带 RETURNING：先 `UPDATE`，`RowsAffected()==0` 再 `INSERT`（见下方 DAO 示例）。
- 时间戳交给 Go 写入（`time.Now().UTC().Format(time.RFC3339)`），存进 `VARCHAR`/`TEXT`，扫回 `time.Time` 时 db 包会自动解析。

应用迁移：

```sh
make migrate DSN=postgres://...                              # 默认 postgres
go run ./cmd/migrate -driver sqlite -dsn file:matex.db
go run ./cmd/migrate -driver mysql  -dsn 'user:pass@tcp(localhost:3306)/matex?parseTime=true'
```

### 3. 写 DAO（internal/domain/<name>/dao.go）

```go
type Row struct {
	ID    int64  `json:"id"    db:"id"`
	Name  string `json:"name"  db:"name"`
	Score int64  `json:"score" db:"score"`
}

func (d *DAO) ByID(ctx context.Context, id int64) (*Row, error) {
	return d.db.QueryOne[Row](ctx, `SELECT id, name, score FROM users WHERE id = ?`, id)
}

// 跨库的"自增"写法：UPDATE 命中 0 行再 INSERT。
func (d *DAO) AddScore(ctx context.Context, name string, delta int64) error {
	res, err := d.db.Exec(ctx, `UPDATE users SET score = score + ? WHERE name = ?`, delta, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_, err = d.db.Exec(ctx, `INSERT INTO users (name, score) VALUES (?, ?)`, name, delta)
	}
	return err
}
```

- `d.db` 类型是 `*db.DB`；`QueryAll[T]` / `QueryOne[T]` / `QueryScalar[T]` / `Exec` 都是它的方法。
- 字段用 `db:"col"` tag 匹配列名（缺省用字段名，大小写不敏感）；查不到的列会被忽略，`SELECT *` 安全。
- 可空列用 `*string` / `*int64` / `sql.NullString` 等。
- **事务里要复用同一个 DAO，方法参数就得收 `db.Querier`**（见下）。

### 3b. 让 DAO 能在事务里跑：参数收 `db.Querier`

```go
func (d *DAO) ByID(ctx context.Context, q db.Querier, id int64) (*Row, error) {
	return db.QueryOne[Row](ctx, q, `SELECT id, name, score FROM users WHERE id = ?`, id)
}
```

`*db.DB` 和 `*db.Tx` 都实现 `db.Querier`，所以同一个方法两处都能用：

```go
row, err := dao.ByID(ctx, env.DB, 1)                      // 池上

err = env.DB.WithTx(ctx, func(ctx context.Context, tx *db.Tx) error {
	row, err := dao.ByID(ctx, tx, 1)                      // 事务内，同一份代码
	if err != nil {
		return err
	}
	_, err = dao.AddScore(ctx, tx, row.Name, 10)          // 复用，不重复写 SQL
	return err
})
```

为什么必须有这个接口：**接口的方法不能带类型参数**（类型可以 —— Go 1.27 起支持；接口不行，
编译器报 `interface method must have no type parameters`），所以 `QueryOne[T]` 只能是包级
函数 `db.QueryOne[T](ctx, q, …)`。

**不要写 `ByID` + `ByIDTx` 两个方法** —— 那正是 `Querier` 要消灭的重复。

### 3c. 事务

```go
err := d.db.WithTx(ctx, func(ctx context.Context, tx *db.Tx) error {
	if _, err := tx.Exec(ctx, `UPDATE users SET score = score + 1 WHERE id = ?`, id); err != nil {
		return err
	}
	return nil   // nil 提交；返回 error 或 panic 自动回滚
})
```

### 4. 测试（无 Docker、无需下载）

`dbtest.Start(t)` 默认起一个**临时 SQLite 文件**（纯 Go，离线可用）并自动跑 migrations：

```go
func TestDAOByID(t *testing.T) {
	d := dbtest.Start(t)      // sqlite；想打真库见下
	dao := NewDAO(d)
	if _, err := dao.ByID(t.Context(), 1); !errors.Is(err, db.ErrNoRow) {
		t.Fatalf("want ErrNoRow, got %v", err)
	}
}
```

要跑真库，用环境变量覆盖（同一套测试）：

```sh
DB_TEST_DRIVER=postgres DB_TEST_DSN='postgres://...' go test ./...
DB_TEST_DRIVER=mysql    DB_TEST_DSN='user:pass@tcp(localhost:3306)/matex?parseTime=true' go test ./...
```

## 约定

- 列名小写下划线，struct 用 `db:"col"` tag。
- 查无此行返回 `db.ErrNoRow`，业务层按需映射成 `errs.NotFound`。
- 写 SQL 一律用 `?`；postgres 的 JSON `?` 操作符请写 `??` 转义，或改用 `jsonb_exists(...)`。
- 测试默认 sqlite、可离线、并行安全（每个测试一个临时文件），所以 `make test-short` 也会跑 DB 用例；CI 无需 Docker。

## 验证

```sh
make ci          # vet + test-short + build（sqlite 跑 DAO 用例）
make test        # 全量测试
make dev         # 需要真库时：docker compose 起 postgres/mysql/redis/memcache/kafka
```
