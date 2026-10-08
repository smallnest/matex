# database — 数据库（PostgreSQL / MySQL / SQLite）

`pkg/core/db` 在 `database/sql` 上做薄封装：**SQL 还是 SQL**，一个方言跑三个驱动。

## 运行

```sh
# 零外部依赖：临时 SQLite（纯 Go，无 cgo）
go run ./examples/database

# 真库（先 make dev）
go run ./examples/database -driver postgres -dsn 'postgres://matex:matex@localhost:5432/matex?sslmode=disable'
go run ./examples/database -driver mysql    -dsn 'matex:matex@tcp(localhost:3306)/matex?parseTime=true'

go test ./examples/database        # 用 dbtest（默认 SQLite，离线可跑）
```

## 关键点

### 三个驱动，一份 SQL

| driver | DSN 示例 |
|---|---|
| `postgres`（默认） | `postgres://user:pass@host:5432/db?sslmode=disable` |
| `mysql` | `user:pass@tcp(host:3306)/db?parseTime=true` |
| `sqlite` | `file:matex.db`（或 `:memory:`） |

**SQL 里一律写 `?`**：postgres 会由 `pkg/core/db` 自动改写成 `$1..$n`。
（postgres 的 JSON 操作符 `?` 要写成 `??`，或用 `jsonb_exists`。）

### 泛型扫描

```go
type Article struct {
	Slug      string    `db:"slug"`
	Views     int64     `db:"views"`
	Note      *string   `db:"note"`       // NULL → nil
	UpdatedAt time.Time `db:"updated_at"` // RFC3339 字符串 → time.Time
}

a, err := d.QueryOne[Article](ctx, `SELECT * FROM articles WHERE slug = ?`, slug)
list, err := d.QueryAll[Article](ctx, `SELECT * FROM articles ORDER BY slug`)
n, err := d.QueryScalar[int64](ctx, `SELECT COUNT(*) FROM articles`)
```

`db` tag 匹配列名（缺省用字段名，大小写不敏感）；**多余的列直接忽略**，
所以 `SELECT *` 在表加列后不会炸。`*T` 指针和 `sql.Null*` 都支持。

### 事务

```go
err := d.WithTx(ctx, func(ctx context.Context, tx *db.Tx) error {
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance = balance - ? WHERE id = ?`, amt, from); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET balance = balance + ? WHERE id = ?`, amt, to); err != nil {
		return err
	}
	return nil // nil 提交；返回 error（或 panic）自动回滚
})
```

`*db.Tx` 和 `*db.DB` 有同一组方法（Exec / QueryAll / QueryOne / QueryScalar）。

### 可移植写法的几条硬规矩

- 主键用 `VARCHAR(n)`，别用 `TEXT`（MySQL 不能索引裸 TEXT 主键）。
- 别依赖 `AUTO_INCREMENT` / `SERIAL` / `IDENTITY`；ID 由应用生成（如 UUID/slug）。
- 时间戳用**字符串从 Go 写入**，不要 `now()` / `CURRENT_TIMESTAMP`。
- upsert 三方言不同（`ON CONFLICT` / `ON DUPLICATE KEY` / `INSERT OR REPLACE`）：
  想跨库就先 `UPDATE`，`RowsAffected()==0` 再 `INSERT`（见 `internal/domain/helloworld/dao.go`）。
- `RETURNING` 只有 postgres/sqlite 有；MySQL 用 `LastInsertId` 或再查一次。

### 迁移

```sh
make migrate DRIVER=sqlite DSN=file:matex.db          # 默认 postgres
go run ./cmd/migrate -driver mysql -dsn '...' -dir migrations
```

`migrations/*.sql` 按**文件名序**执行，已执行的记在 `schema_migrations` 表里；
**追加新文件，不要改老文件**。
