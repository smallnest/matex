package helloworld

import (
	"testing"

	"github.com/smallnest/matex/pkg/core/redistest"
)

func TestGreetCaching(t *testing.T) {
	rc := redistest.Start(t)
	svc := New(Deps{Redis: rc, Greeting: "Hi "})

	ctx := t.Context()
	r1, err := svc.Greet(ctx, "world")
	if err != nil {
		t.Fatal(err)
	}
	if r1.Cached {
		t.Fatal("first call should not be cached")
	}
	if r1.Greeting != "Hi world!" {
		t.Fatalf("greeting: %q", r1.Greeting)
	}

	r2, err := svc.Greet(ctx, "world")
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Cached || r2.Greeting != "Hi world!" {
		t.Fatalf("second call should hit cache: %+v", r2)
	}
}

func TestGreetNoDeps(t *testing.T) {
	svc := New(Deps{Greeting: "Hello, "})
	r, err := svc.Greet(t.Context(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if r.Greeting != "Hello, x!" || r.Cached || r.Count != 0 {
		t.Fatalf("unexpected: %+v", r)
	}
}

func TestStatsNoDB(t *testing.T) {
	svc := New(Deps{})
	_, err := svc.Stats(t.Context(), "x")
	if err == nil {
		t.Fatal("expected unavailable error")
	}
}
