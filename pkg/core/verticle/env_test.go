package verticle

import (
	"testing"

	"github.com/smallnest/matex/pkg/core/dbtest"
)

// The accessor contract is the seam between the framework and a domain:
// RequireX fails the boot when the config section is absent, X reports
// availability with a comma-ok. These tests pin that contract so a
// future change cannot silently reintroduce the nil-check convention.

func TestRequireDBAbsent(t *testing.T) {
	e := &Env{}
	if _, err := e.RequireDB(); err == nil {
		t.Fatal("RequireDB on an Env without db must return an error")
	}
	if dbc, ok := e.DB(); ok || dbc != nil {
		t.Fatalf("DB() on an Env without db: got (%v, %v), want (nil, false)", dbc, ok)
	}
}

func TestRequireDBPresent(t *testing.T) {
	d := dbtest.Start(t)
	e := &Env{db: d}
	got, err := e.RequireDB()
	if err != nil {
		t.Fatalf("RequireDB on an Env with db: %v", err)
	}
	if got != d {
		t.Fatal("RequireDB returned a different handle than the one set")
	}
	if dbc, ok := e.DB(); !ok || dbc != d {
		t.Fatalf("DB() on an Env with db: got (%v, %v), want (d, true)", dbc, ok)
	}
}

func TestOptionalAccessorsAbsent(t *testing.T) {
	e := &Env{}
	if _, err := e.RequireRedis(); err == nil {
		t.Error("RequireRedis on an Env without redis must return an error")
	}
	if _, err := e.RequireMemcache(); err == nil {
		t.Error("RequireMemcache on an Env without memcache must return an error")
	}
	if _, err := e.RequireKafka(); err == nil {
		t.Error("RequireKafka on an Env without kafka must return an error")
	}
	for _, name := range []string{"redis", "memcache", "kafka"} {
		var ok bool
		switch name {
		case "redis":
			_, ok = e.Redis()
		case "memcache":
			_, ok = e.Memcache()
		case "kafka":
			_, ok = e.Kafka()
		}
		if ok {
			t.Errorf("%s: accessor reported available on an Env without it", name)
		}
	}
}
