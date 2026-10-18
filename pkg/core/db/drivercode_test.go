package db

import (
	"errors"
	"testing"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/smallnest/matex/pkg/core/errs"
)

// driverCode must recognize every driver's unique-violation code, wrapped or
// not, and stay silent on anything else — a wrong "conflict" would turn a
// real failure into a misleading 409.

type fakeSQLState string

func (f fakeSQLState) Error() string    { return string(f) }
func (f fakeSQLState) SQLState() string { return string(f) }

type fakeCode int

func (f fakeCode) Error() string { return "fake sqlite error" }
func (f fakeCode) Code() int     { return int(f) }

func TestDriverCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"postgres unique", fakeSQLState("23505"), "23505"},
		{"postgres other", fakeSQLState("42P01"), ""},
		{"sqlite unique extended", fakeCode(2067), "2067"},
		{"sqlite other", fakeCode(19), ""},
		{"mysql duplicate key", &mysql.MySQLError{Number: 1062}, "1062"},
		{"mysql other", &mysql.MySQLError{Number: 9999}, ""},
		{"wrapped mysql duplicate", errors.Join(errors.New("exec"), &mysql.MySQLError{Number: 1062}), "1062"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := driverCode(c.err); got != c.want {
				t.Fatalf("driverCode() = %q", got)
			}
		})
	}
}

func TestWrapErrMapsUniqueToConflict(t *testing.T) {
	err := wrapErr(&mysql.MySQLError{Number: 1062})
	if errs.KindOf(err) != errs.KindConflict {
		t.Fatalf("KindOf = %v, want KindConflict", errs.KindOf(err))
	}
	if errs.KindOf(wrapErr(&mysql.MySQLError{Number: 1064})) != errs.KindInternal {
		t.Fatalf("a syntax error must stay internal")
	}
}
