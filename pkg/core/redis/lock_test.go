package redis_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/smallnest/matex/pkg/core/redis"
	"github.com/smallnest/matex/pkg/core/redistest"
)

// startClients returns two clients on ONE Redis. redistest.Start starts a
// fresh miniredis per call, so two Start(t) calls would each get their own
// server — contention tests need the same server to prove exclusion. rest
// holds five more clients on the same server (fairness test).
func startClients(t *testing.T) (first *redis.Client, rest []*redis.Client) {
	t.Helper()
	addr := redistest.Addr(t)
	client := func() *redis.Client {
		g := goredis.NewClient(&goredis.Options{Addr: addr})
		t.Cleanup(func() { _ = g.Close() })
		return redis.New(g)
	}
	first = client()
	for i := 0; i < 5; i++ {
		rest = append(rest, client())
	}
	return first, rest
}

// The lock helpers in lock.go are the primitives examples/redis and any
// future cron/idempotency upgrade build on, so their contract is pinned
// here against miniredis — same policy as TryLock/Claim in redis_test.go.

func TestLockBlocksUntilReleased(t *testing.T) {
	rc, others := startClients(t)
	rc2 := others[0]
	ctx := t.Context()

	a, err := rc.Lock(ctx, "job")
	if err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	defer a.Release(ctx)

	// B waits on a second client: same-client Lock is reentrancy by
	// design (it shares the Locker and returns at once).
	acquired := make(chan struct{})
	go func() {
		b, err := rc2.Lock(ctx, "job")
		if err != nil {
			t.Errorf("B acquire: %v", err)
			return
		}
		close(acquired)
		_ = b.Release(ctx)
	}()

	// B must still be waiting while A holds the lock.
	select {
	case <-acquired:
		t.Fatal("B acquired a held lock")
	case <-time.After(150 * time.Millisecond):
	}

	if err := a.Release(ctx); err != nil {
		t.Fatalf("A release: %v", err)
	}
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("B never acquired the lock after A released it")
	}
}

func TestLockWaitTimeout(t *testing.T) {
	rc, others := startClients(t)
	rc2 := others[0]
	ctx := t.Context()

	a, err := rc.Lock(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = rc2.Lock(ctx, "job", redis.WithWaitTimeout(150*time.Millisecond))
	if !errors.Is(err, redis.ErrLockBusy) {
		t.Fatalf("err = %v, want ErrLockBusy", err)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("gave up after %v, waited far too little", d)
	}

	// The give-up path must not leave the waiter in the queue: after A
	// releases, a fresh Lock must acquire without delay.
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := rc2.Lock(ctx, "job", redis.WithWaitTimeout(time.Second))
	if err != nil {
		t.Fatalf("fresh acquire after timeout-and-release: %v", err)
	}
	_ = b.Release(ctx)
}

func TestLockReentrant(t *testing.T) {
	rc, _ := startClients(t)
	ctx := t.Context()

	a, err := rc.Lock(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	// Same client, same key: nests, and a second holder must not be able
	// to slip in between the two levels.
	b, err := rc.Lock(ctx, "job") // same client → same Locker, nests
	if err != nil {
		t.Fatalf("reentrant acquire: %v", err)
	}
	// One release drops one level; the key survives until the last one.
	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rc.Cmdable().Exists(ctx, "lock:job").Val(); n != 1 {
		t.Fatal("first release deleted a nested lock")
	}
	if err := b.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rc.Cmdable().Exists(ctx, "lock:job").Val(); n != 0 {
		t.Fatal("last release left the key behind")
	}
}

func TestLockRenewKeepsTTL(t *testing.T) {
	rc, others := startClients(t)
	rc2 := others[0]
	ctx := t.Context()

	// A short TTL with an active watchdog: the key must outlive the TTL
	// many times over, and no outsider may acquire it meanwhile.
	l, err := rc.Lock(ctx, "job", redis.WithLockTTL(300*time.Millisecond), redis.WithWatchdog(200*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n := rc.Cmdable().Exists(ctx, "lock:job").Val(); n != 1 {
			t.Fatal("lock key vanished while the watchdog was renewing")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := rc2.Lock(ctx, "job", redis.WithWaitTimeout(50*time.Millisecond)); !errors.Is(err, redis.ErrLockBusy) {
		t.Fatalf("outsider acquired a renewed lock: %v", err)
	}
}

func TestLockRenewReportsLoss(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	l, err := rc.Lock(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}
	// Losing the key under the holder makes a manual Renew report the loss
	// and close Lost(), the same signal the watchdog gives.
	if err := rc.Del(ctx, "lock:job"); err != nil {
		t.Fatal(err)
	}
	if err := l.Renew(ctx); !errors.Is(err, redis.ErrLockLost) {
		t.Fatalf("renew after loss = %v, want ErrLockLost", err)
	}
	select {
	case <-l.Lost():
	case <-time.After(time.Second):
		t.Fatal("Lost() not closed after manual Renew found the key missing")
	}
}

func TestLockLostOnExpiry(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	l, err := rc.Lock(ctx, "job", redis.WithLockTTL(30*time.Second), redis.WithWatchdog(300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the worst case directly: the key disappears under the
	// holder (failover took longer than the TTL, someone DELeted it).
	if err := rc.Del(ctx, "lock:job"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.Lost():
		// The watchdog must have noticed within one tick.
	case <-time.After(5 * time.Second):
		t.Fatal("Lost() never fired after the key was deleted")
	}
}

func TestLockFairness(t *testing.T) {
	rc, others := startClients(t)
	ctx := t.Context()

	a, err := rc.Lock(ctx, "job")
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan string, 2)
	wait := func(id string, c *redis.Client) {
		go func() {
			l, err := c.Lock(ctx, "job")
			if err != nil {
				t.Errorf("%s acquire: %v", id, err)
				return
			}
			acquired <- id
			_ = l.Release(ctx)
		}()
	}
	recv := func() string {
		select {
		case id := <-acquired:
			return id
		case <-time.After(5 * time.Second):
			t.Fatal("waiter never acquired the lock")
			return ""
		}
	}

	// B joins the queue first, then C, so B's ticket is ahead of C's. The
	// queue head must take a freed lock first, or a tail could cut in.
	wait("B", others[0])
	waitQueueLen(t, rc, "lock:job:queue", 1)
	wait("C", others[1])
	waitQueueLen(t, rc, "lock:job:queue", 2)

	if err := a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if first, second := recv(), recv(); first != "B" || second != "C" {
		t.Fatalf("FIFO violated: %s then %s, want B then C", first, second)
	}
}

// waitQueueLen blocks until the waiter queue beside key holds exactly want
// tickets, proving the waiters joined in arrival order.
func waitQueueLen(t *testing.T, rc *redis.Client, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		q, err := rc.Cmdable().Get(t.Context(), key).Result()
		if err == nil && len(strings.Split(q, ",")) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("queue %s never reached %d entries", key, want)
}

func TestTryLockStillOneShot(t *testing.T) {
	rc := redistest.Start(t)
	ctx := t.Context()

	// The historical signature keeps its contract: one attempt, no wait.
	release, ok, err := rc.TryLock(ctx, "job", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first: ok=%v err=%v", ok, err)
	}
	_, ok, err = rc.TryLock(ctx, "job", time.Minute)
	if err != nil || ok {
		t.Fatalf("second: ok=%v err=%v, want busy", ok, err)
	}
	if err := release(ctx); err != nil {
		t.Fatal(err)
	}
}
