// Command errors shows the matex error model (pkg/core/errs).
//
// Every error carries a Kind (mapped to an HTTP status), a business Code
// and a human message. The HTTP layer renders them uniformly as
// {"code":<code>,"msg":"<msg>","data":null}; the response body never
// leaks the wrapped cause — that lives in the logs.
//
// Run: go run ./examples/errors
package main

import (
	"errors"
	"fmt"

	"github.com/smallnest/matex/pkg/core/errs"
)

func main() {
	// ── 1. Kind → HTTP 状态 ────────────────────────────────────────────
	fmt.Println("Kind                → HTTP")
	for _, c := range kinds() {
		fmt.Printf("  %-18s → %d\n", c.name, c.kind.HTTPStatus())
	}

	// ── 2. 快捷构造 + Status 提取 ─────────────────────────────────────
	fmt.Println("\nerrs.Status(err) → (status, code, msg)")
	for _, err := range constructors() {
		status, code, msg := errs.Status(err)
		fmt.Printf("  %-3d %-6d %s\n", status, code, msg)
	}

	// ── 3. 包装：errors.As 能穿透，响应只给 msg ────────────────────────
	fmt.Println("\n包装（errs.Wrap / fmt.Errorf %w）:")
	base := errs.Wrap(errs.KindNotFound, 40401, errors.New("sql: no rows in result set"), "user 42 not found")
	wrapped := fmt.Errorf("handler: %w", base) // 普通 %w 包装也能被穿透
	status, code, msg := errs.Status(wrapped)
	fmt.Printf("  穿透 fmt.Errorf: status=%d code=%d msg=%q\n", status, code, msg)
	if e, ok := errors.AsType[*errs.Error](wrapped); ok {
		fmt.Printf("  取出 *errs.Error: Kind=%d Cause=%v（只进日志）\n", e.Kind, e.Cause)
	}

	dbErr := errs.Wrap(errs.KindConflict, 40901,
		errors.New(`pq: duplicate key value violates unique constraint "users_name_key"`), "创建用户失败")
	status, code, msg = errs.Status(dbErr)
	fmt.Printf("  Wrap(cause)    : Error()=%q\n", dbErr.Error())
	fmt.Printf("                   对客户端只回: status=%d code=%d msg=%q\n", status, code, msg)

	// ── 4. code 省略（0）时回退为 status*100 ───────────────────────────
	fmt.Println("\ncode=0 时回退为 状态码*100:")
	_, code, msg = errs.Status(errs.New(errs.KindConflict, 0, "已存在"))
	fmt.Printf("  KindConflict, code=0 → code=%d msg=%q\n", code, msg)

	// ── 5. 普通 error 一律 500，且不泄露细节 ───────────────────────────
	fmt.Println("\n非 matex 错误:")
	status, code, msg = errs.Status(errors.New("internal detail nobody should see"))
	fmt.Printf("  status=%d code=%d msg=%q\n", status, code, msg)
}

func kinds() []struct {
	name string
	kind errs.Kind
} {
	return []struct {
		name string
		kind errs.Kind
	}{
		{"invalid_argument", errs.KindInvalidArgument},
		{"unauthorized", errs.KindUnauthorized},
		{"forbidden", errs.KindForbidden},
		{"not_found", errs.KindNotFound},
		{"conflict", errs.KindConflict},
		{"rate_limited", errs.KindRateLimited},
		{"internal", errs.KindInternal},
		{"unavailable", errs.KindUnavailable},
		{"timeout", errs.KindTimeout},
	}
}

func constructors() []error {
	return []error{
		errs.Invalid(40001, "name is required"),
		errs.Unauthorized(40101, "token expired"),
		errs.Forbidden(40301, "not your resource"),
		errs.NotFound(40401, "user %d not found", 42),
		errs.Conflict(40901, "name already taken"),
		errs.RateLimited(42901, "too many requests"),
		errs.Internal(50001, "boom"),
		errs.Unavailable(50301, "database is not configured"),
		errs.Timeout(50401, "upstream timeout"),
	}
}
