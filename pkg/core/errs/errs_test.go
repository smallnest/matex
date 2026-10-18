package errs

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   int
	}{
		{NotFound(40401, "user %d not found", 1), 404, 40401},
		{Invalid(40002, "bad input"), 400, 40002},
		{Unauthorized(40100, "no token"), 401, 40100},
		{Timeout(50400, "timeout"), 504, 50400},
		{New(KindInternal, 0, "boom"), 500, 50000}, // code 0 → status*100
		{errors.New("plain"), 500, 50000},          // non-matex → internal
	}
	for _, c := range cases {
		status, code, _ := Status(c.err)
		if status != c.status || code != c.code {
			t.Errorf("Status(%v) = (%d,%d), want (%d,%d)", c.err, status, code, c.status, c.code)
		}
	}
}

func TestUnwrapAndIs(t *testing.T) {
	cause := errors.New("pg: timeout")
	wrapped := Wrap(KindInternal, 50001, cause, "save failed")
	if !errors.Is(wrapped, cause) {
		t.Fatal("errors.Is should traverse the cause")
	}
	// survives a %w re-wrap
	outer := fmt.Errorf("outer: %w", wrapped)
	if !errors.Is(outer, cause) {
		t.Fatal("errors.Is should traverse through fmt wrapping")
	}
	if KindOf(outer) != KindInternal {
		t.Fatalf("KindOf: %v", KindOf(outer))
	}
}

func TestErrorString(t *testing.T) {
	e := Newf(KindNotFound, 40400, "missing %s", "key")
	if e.Error() != "missing key" {
		t.Fatalf("Error(): %q", e.Error())
	}
	w := Wrap(KindInternal, 50000, errors.New("x"), "boom")
	if w.Error() != "boom: x" {
		t.Fatalf("Error(): %q", w.Error())
	}
}

// A handler that returns ctx.Err() must not be reported as an internal
// failure: cancellation and deadline are distinct outcomes.
func TestBareContextErrorsKeepTheirMeaning(t *testing.T) {
	status, code, _ := Status(context.DeadlineExceeded)
	if status != 504 || code != 50400 || KindOf(context.DeadlineExceeded) != KindTimeout {
		t.Fatalf("deadline: %d %d %v", status, code, KindOf(context.DeadlineExceeded))
	}
	status, code, _ = Status(fmt.Errorf("load user: %w", context.Canceled))
	if status != 503 || code != 50300 || KindOf(context.Canceled) != KindUnavailable {
		t.Fatalf("canceled: %d %d %v", status, code, KindOf(context.Canceled))
	}
	if status, _, _ := Status(errors.New("boom")); status != 500 {
		t.Fatalf("plain error status = %d, want 500", status)
	}
}
