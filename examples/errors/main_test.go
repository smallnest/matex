package main

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/smallnest/matex/pkg/core/errs"
)

func TestKindStatusMapping(t *testing.T) {
	want := map[errs.Kind]int{
		errs.KindInvalidArgument: 400,
		errs.KindUnauthorized:    401,
		errs.KindForbidden:       403,
		errs.KindNotFound:        404,
		errs.KindConflict:        409,
		errs.KindRateLimited:     429,
		errs.KindInternal:        500,
		errs.KindUnavailable:     503,
		errs.KindTimeout:         504,
	}
	for _, c := range kinds() {
		if got := c.kind.HTTPStatus(); got != want[c.kind] {
			t.Errorf("%s: status %d, want %d", c.name, got, want[c.kind])
		}
	}
}

func TestStatusThroughWrapping(t *testing.T) {
	wrapped := fmt.Errorf("handler: %w", errs.NotFound(40401, "user %d not found", 42))
	status, code, msg := errs.Status(wrapped)
	if status != http.StatusNotFound || code != 40401 || msg != "user 42 not found" {
		t.Fatalf("got (%d,%d,%q)", status, code, msg)
	}
}

func TestCodeFallback(t *testing.T) {
	_, code, _ := errs.Status(errs.New(errs.KindConflict, 0, "dup"))
	if code != 40900 {
		t.Fatalf("code %d, want 40900", code)
	}
}

func TestWrapKeepsCauseOutOfResponse(t *testing.T) {
	cause := errors.New("pq: duplicate key")
	err := errs.Wrap(errs.KindConflict, 40901, cause, "创建用户失败")
	if got := err.Error(); got != "创建用户失败: pq: duplicate key" {
		t.Fatalf("Error() = %q", got)
	}
	status, code, msg := errs.Status(err)
	if status != http.StatusConflict || code != 40901 || msg != "创建用户失败" {
		t.Fatalf("Status leaked the cause: (%d,%d,%q)", status, code, msg)
	}
	if !errors.Is(err, cause) {
		t.Fatal("errors.Is should find the cause")
	}
}

func TestPlainErrorIsInternal(t *testing.T) {
	status, code, msg := errs.Status(errors.New("secret detail"))
	if status != http.StatusInternalServerError || code != 50000 || msg != "internal error" {
		t.Fatalf("got (%d,%d,%q)", status, code, msg)
	}
}
