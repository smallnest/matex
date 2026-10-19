// Distributed locking: a reentrant, auto-renewed lock backed by one
// Redis key. Business code uses Client.Lock / Client.TryLock instead of
// hand-rolling SETNX + Lua.
package redis

// The lock value is NOT JSON: cjson decode/encode round trips are slow in
// Redis and the value is write-heavy (every acquire, reenter, renew). Two
// plain strings instead, under one key so every check-and-mutate is a
// single script:
//
//	lock:<key>        = "<token>\n<n>"          owner token and reentrancy count
//	lock:<key>:queue  = "<ticket>,<ticket>,..." waiters in arrival order
//
// Fairness is FIFO and cooperative: arriving waiters join the queue beside
// the lock, and only the head ticket may take a lock that falls free, so a
// new arrival cannot jump the line. A crashed waiter's ticket is dead
// weight, not a wedged waiter — the shared queue key expires after one TTL,
// and the next head then takes over. (Why not Pub/Sub wakeups: a lost
// wakeup silently breaks mutual exclusion, while a short poll cannot. Why
// not a JWT-style token: the string compare is enough, and strings cannot
// be confused by a corrupted value.)
//
// The queue lives beside the key and is re-armed by every enqueue, renewal
// and takeover, so its lifetime tracks the lock's. A deadline on the lock
// therefore bounds the queue too: a waiter that dies mid-wait cannot wedge
// anyone past the TTL.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// Default knobs for Lock. Override per call with the With* options.
const (
	DefaultLockTTL      = 30 * time.Second
	defaultPollInterval = 50 * time.Millisecond
)

// ErrLockBusy is returned by Lock when the wait budget runs out before the
// key becomes free.
var ErrLockBusy = errors.New("redis: lock busy (wait timeout elapsed)")

// ErrLockLost is returned by Renew when the key is no longer ours: the
// lease lapsed (lost failover, long pause) and someone else may hold it.
var ErrLockLost = errors.New("redis: lock lost (lease lapsed)")

// Locker is a distributed lock. Acquire it with (*Client).Lock or
// (*Client).TryLock; Release must be called once per successful
// acquisition. One Locker per key per client is shared, which is what
// makes nesting work — the released value is the same object.
type Locker struct {
	c     *Client
	key   string // caller's key, for messages
	token string // owner token; set when the outer acquisition succeeds

	mu        sync.Mutex
	acqs      []*acquisition // held levels, outermost first
	lost      chan struct{}  // built at construction; closed when the lock is lost
	lostOnce  sync.Once      // guards the close of lost against double-report
	watchStop chan struct{}
	ttl       time.Duration
	lastErr   error
}

// newLocker builds a Locker with its lost channel already in place, so
// Lost() is subscribable before any acquisition.
func newLocker(c *Client, key string) *Locker {
	return &Locker{c: c, key: key, token: randomToken(), lost: make(chan struct{})}
}

// acquisition is one Lock call's ticket: it names the queue entry so the
// caller can leave the queue without disturbing nested calls.
type acquisition struct {
	ticket string // queue entry written to Redis
}

var lockRegistry sync.Map // registryKey -> *Locker

type registryKey struct {
	c   *Client
	key string
}

// LockerFor returns the Locker for key on this client, creating it on
// first use. It does not acquire anything.
func (c *Client) LockerFor(key string) *Locker {
	k := registryKey{c: c, key: key}
	if v, ok := lockRegistry.Load(k); ok {
		return v.(*Locker)
	}
	v, _ := lockRegistry.LoadOrStore(k, newLocker(c, key))
	return v.(*Locker)
}

// storageKey namespaces lock keys inside the redis keyspace. Without it a
// lock named "cache:x" would collide with a cache entry of the same name:
// the lock value is private bookkeeping, not the caller's data.
func storageKey(key string) string { return "lock:" + key }

// lockOption tunes one Lock call.
type lockOption func(*lockParams)

// lockParams are the per-acquisition settings. The Lock* prefix is
// deliberate: TTL and watchdog belong to the Locker (one lease per client
// per key), so passing them again on a nested Lock is a programming
// error rather than a silent reconfiguration of someone else's lease.
type lockParams struct {
	ttl          time.Duration
	wait         time.Duration // 0 = only ctx bounds the wait
	pollInterval time.Duration
	watchdog     time.Duration // 0 = none
}

// WithLockTTL sets the lease length for a fresh acquisition (default 30s).
// With the watchdog on, this is how long the lease survives one missed
// renewal, not a deadline. It applies only when this call creates the
// lease; nested Lock calls reject it.
func WithLockTTL(d time.Duration) lockOption { return func(p *lockParams) { p.ttl = d } }

// WithWaitTimeout caps how long Lock blocks on a busy key (default:
// bounded only by ctx). When it elapses Lock returns ErrLockBusy.
func WithWaitTimeout(d time.Duration) lockOption {
	return func(p *lockParams) { p.wait = d }
}

// WithPollInterval sets the retry cadence for the poll loop (default
// 50ms). Every waiter polls at this rate; only the head may acquire, so a
// faster poll does not let a tail jump the queue.
func WithPollInterval(d time.Duration) lockOption {
	return func(p *lockParams) { p.pollInterval = d }
}

// WithWatchdog re-arms the lease (and the waiter queue's TTL) every
// watchdog/3 while this client holds the lock, so a critical section may
// run longer than the TTL. If a renewal fails — the key lapsed after a
// failover or a long pause, and may be held by someone else — Lock lets the
// holder know via Lost() and Renew returns ErrLockLost. Disabled by default
// so callers pay for the extra goroutine only when they ask. Like
// WithLockTTL it applies only when this call creates the lease.
func WithWatchdog(interval time.Duration) lockOption {
	return func(p *lockParams) { p.watchdog = interval }
}

// Lock blocks until the lock is free, the wait budget (WithWaitTimeout)
// runs out (ErrLockBusy), or ctx is done. Nesting: calling Lock again for
// a key this client already holds increments the count and returns the
// same *Locker immediately.
func (c *Client) Lock(ctx context.Context, key string, opts ...lockOption) (*Locker, error) {
	p := lockParams{ttl: DefaultLockTTL, pollInterval: defaultPollInterval}
	for _, o := range opts {
		o(&p)
	}
	l := c.LockerFor(key)
	l.mu.Lock()
	nested := len(l.acqs) > 0
	l.mu.Unlock()
	if nested {
		// One lease per client and key: reentering must not redefine it.
		// Accepting a second TTL here would let whichever nested call
		// released last decide when the key expires.
		if p.ttl != DefaultLockTTL || p.watchdog != 0 {
			return nil, errors.New("redis: lock " + key + " already held by this client; TTL and watchdog apply to the outer Lock only")
		}
		// Book one level per Lock call so each matched Release drops
		// exactly one; the Redis-side count is re-armed by the script so
		// the key outlives every level but the last.
		if _, err := l.acquireOnce(ctx); err != nil {
			return nil, err
		}
		return l, nil
	}
	if err := l.checkHealthy(); err != nil {
		return nil, err
	}
	if err := l.acquirePolling(ctx, p); err != nil {
		return nil, err
	}
	// The watchdog cares only about a held lease. A queued waiter has
	// nothing to renew — the owner's renewals keep the shared queue alive —
	// so it starts after acquisition, not before the wait.
	if p.watchdog > 0 {
		l.startWatchdog(p)
	}
	return l, nil
}

// TryLock makes exactly one attempt and never waits or nests: ok is false
// when any client — including this one — holds the key. The returned
// release function is the historical token-checked delete, so a lease
// that lapsed can never be released by its stale holder. It keeps the raw
// key (the historical contract) rather than Lock's "lock:" namespace, so a
// TryLock and a Lock on the same name are independent locks.
func (c *Client) TryLock(ctx context.Context, key string, ttl time.Duration) (release func(context.Context) error, ok bool, err error) {
	token := randomToken()
	sk := key
	n, err := tryAcquireScript.RunInt(ctx, c, []string{sk}, token, strconv.FormatInt(ttl.Milliseconds(), 10))
	if err != nil {
		return nil, false, fmt.Errorf("redis: lock %s: %w", key, err)
	}
	if n != 1 {
		return nil, false, nil
	}
	return func(ctx context.Context) error {
		if _, err := tryReleaseScript.RunInt(ctx, c, []string{sk}, token); err != nil {
			return fmt.Errorf("redis: release %s: %w", key, err)
		}
		return nil
	}, true, nil
}

// tryAcquireScript is TryLock's SETNX-with-token: succeeds only when no
// key exists at all, so TryLock sees any owner as busy.
var tryAcquireScript = NewScript(`
if redis.call("exists", KEYS[1]) == 1 then return 0 end
redis.call("set", KEYS[1], ARGV[1])
redis.call("pexpire", KEYS[1], ARGV[2])
return 1
`)

// tryReleaseScript deletes only with the caller's token still in place.
var tryReleaseScript = NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// The five scripts below implement the whole state machine against the
// two-string layout:
//
//	lock:<key>       "<token>\n<n>"     owner + reentrancy count
//	lock:<key>:queue "t1,t2,..."        waiting tickets, FIFO
//
// Every one of them is atomic in Redis, so a ticket can never exist
// without its queue entry, a count can never drop twice, and a release
// can never delete a lock that changed hands.

// ticketScript acquires or enqueues. ARGV: token, ticket, ttl-ms.
// Returns {1, 0} when the caller now holds the lock (fresh or reentrant),
// {0, n} when its ticket was appended to the queue (or already sat there).
// A free lock is taken only when the queue is empty, so a fresh arrival
// never jumps the waiters the last holder left behind.
var ticketScript = NewScript(`
local v = redis.call("get", KEYS[1])
if v then
	local token = string.match(v, "^(.-)\n")
	if token == ARGV[1] then
		local n = tonumber(string.match(v, "\n(%d+)$")) + 1
		redis.call("set", KEYS[1], ARGV[1] .. "\n" .. n)
		redis.call("pexpire", KEYS[1], ARGV[3])
		return {1, 0}
	end
else
	local q = redis.call("get", KEYS[2])
	if not q or q == "" then
		redis.call("set", KEYS[1], ARGV[1] .. "\n1")
		redis.call("pexpire", KEYS[1], ARGV[3])
		return {1, 0}
	end
end
local q = redis.call("get", KEYS[2])
local entry = ARGV[2]
if q and string.find("," .. q .. ",", "," .. entry .. ",", 1, true) then
	return {0, 0}
end
if q then q = q .. "," .. entry else q = entry end
redis.call("set", KEYS[2], q)
redis.call("pexpire", KEYS[2], ARGV[3])
return {0, 1}
`)

// waitScript polls once from inside Redis. A free lock goes only to the
// head ticket, which drops itself from the queue and re-arms the rest; the
// lock may have expired between polls and this script then hands it over in
// queue order (an expired lock has no owner to be fair to). ARGV: token,
// ticket, ttl-ms.
var waitScript = NewScript(`
local v = redis.call("get", KEYS[1])
if not v then
	local q = redis.call("get", KEYS[2])
	if q then
		local head = string.match(q, "^([^,]*)")
		if head ~= ARGV[2] then return 0 end
		local rest = string.match(q, "^[^,]*,?(.*)$")
		if rest ~= "" then
			redis.call("set", KEYS[2], rest)
			redis.call("pexpire", KEYS[2], ARGV[3])
		else
			redis.call("del", KEYS[2])
		end
	end
	redis.call("set", KEYS[1], ARGV[1] .. "\n1")
	redis.call("pexpire", KEYS[1], ARGV[3])
	return 1
end
local token = string.match(v, "^(.-)\n")
if token == ARGV[1] then
	local n = tonumber(string.match(v, "\n(%d+)$")) + 1
	redis.call("set", KEYS[1], ARGV[1] .. "\n" .. n)
	redis.call("pexpire", KEYS[1], ARGV[3])
	return 1
end
return 0
`)

// releaseScript drops one nesting level. Returns 1 released, 0 not ours
// (expired or taken over) — a stale release must never delete a new
// owner's lock. On the last level it frees the lock but leaves the waiter
// queue, so the head ticket takes over in order instead of a first-poller
// winning. ARGV: token, ttl-ms.
var releaseScript = NewScript(`
local v = redis.call("get", KEYS[1])
if not v then return 0 end
local token = string.match(v, "^(.-)\n")
if token ~= ARGV[1] then return 0 end
local n = tonumber(string.match(v, "\n(%d+)$")) - 1
if n > 0 then
	redis.call("set", KEYS[1], ARGV[1] .. "\n" .. n)
	redis.call("pexpire", KEYS[1], ARGV[2])
	return 1
end
redis.call("del", KEYS[1])
return 0
`)

// renewScript extends the lease (and the waiter queue it shields) when the
// caller still owns it.
var renewScript = NewScript(`
local v = redis.call("get", KEYS[1])
if not v then return 0 end
local token = string.match(v, "^(.-)\n")
if token ~= ARGV[1] then return 0 end
redis.call("pexpire", KEYS[1], ARGV[2])
redis.call("pexpire", KEYS[2], ARGV[2])
return 1
`)

// departScript removes this call's ticket from the queue, whichever
// position it holds (a timeout can fire while still queued).
var departScript = NewScript(`
local q = redis.call("get", KEYS[1])
if not q then return 0 end
local out = {}
local removed = false
for s in string.gmatch(q, "[^,]+") do
	if s == ARGV[1] then removed = true else out[#out + 1] = s end
end
if not removed then return 0 end
if #out == 0 then
	redis.call("del", KEYS[1])
else
	redis.call("set", KEYS[1], table.concat(out, ","))
end
return 1
`)

// acquireOnce makes one acquisition attempt for an already-held locker:
// the script sees our token and bumps the count. The queue bookkeeping is
// skipped — a holder cannot wait behind itself.
func (l *Locker) acquireOnce(ctx context.Context) (bool, error) {
	sk := storageKey(l.key)
	l.mu.Lock()
	ttlMs := strconv.FormatInt(l.ttl.Milliseconds(), 10)
	l.mu.Unlock()
	vals, err := ticketScript.Run(ctx, l.c, []string{sk, sk + ":queue"}, l.token, "", ttlMs).Slice()
	if err != nil {
		return false, fmt.Errorf("redis: lock %s: %w", l.key, err)
	}
	n, _ := vals[0].(int64)
	if n != 1 {
		return false, nil
	}
	l.mu.Lock()
	l.acqs = append(l.acqs, &acquisition{ticket: ""})
	l.mu.Unlock()
	return true, nil
}

// acquirePolling runs the acquire/queue loop. When it returns nil the
// caller is in l.acqs; otherwise it is neither holding nor queued.
func (l *Locker) acquirePolling(ctx context.Context, p lockParams) error {
	acq := &acquisition{ticket: l.token + "|" + strconv.FormatUint(randomToken64(), 36)}
	sk := storageKey(l.key)
	skq := sk + ":queue"
	ttlMs := strconv.FormatInt(p.ttl.Milliseconds(), 10)
	interval := p.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}

	deadline := time.Time{}
	if p.wait > 0 {
		deadline = time.Now().Add(p.wait)
	}

	l.mu.Lock()
	l.ttl = p.ttl
	l.mu.Unlock()
	queued, err := l.enqueue(ctx, acq, sk, skq, ttlMs)
	if err != nil {
		return err
	}

	for !queued {
		if !deadline.IsZero() && time.Now().After(deadline) {
			_, _ = departScript.RunInt(context.WithoutCancel(ctx), l.c, []string{skq}, acq.ticket)
			return ErrLockBusy
		}
		if ctx.Err() != nil {
			_, _ = departScript.RunInt(context.WithoutCancel(ctx), l.c, []string{skq}, acq.ticket)
			return ctx.Err()
		}
		wait := interval
		if !deadline.IsZero() {
			if left := time.Until(deadline); left < wait {
				wait = left
			}
		}
		if wait < time.Millisecond {
			wait = time.Millisecond
		}

		select {
		case <-ctx.Done():
			_, _ = departScript.RunInt(context.WithoutCancel(ctx), l.c, []string{skq}, acq.ticket)
			return ctx.Err()
		case <-time.After(wait):
		}

		n, err := waitScript.RunInt(ctx, l.c, []string{sk, skq}, l.token, acq.ticket, ttlMs)
		if err != nil {
			_, _ = departScript.RunInt(context.WithoutCancel(ctx), l.c, []string{skq}, acq.ticket)
			return fmt.Errorf("redis: lock %s: %w", l.key, err)
		}
		if n == 1 {
			break
		}
		queued, err = l.enqueue(ctx, acq, sk, skq, ttlMs)
		if err != nil {
			return err
		}
	}
	l.mu.Lock()
	l.acqs = append(l.acqs, acq)
	l.mu.Unlock()
	return nil
}

// enqueue runs one acquire-or-enqueue step. When it reports true the
// caller holds the lock; otherwise the ticket is in the queue and the
// caller may wait until it reaches the head.
func (l *Locker) enqueue(ctx context.Context, acq *acquisition, sk, skq string, ttlMs string) (bool, error) {
	vals, err := ticketScript.Run(ctx, l.c, []string{sk, skq}, l.token, acq.ticket, ttlMs).Slice()
	if err != nil {
		return false, fmt.Errorf("redis: lock %s: %w", l.key, err)
	}
	held, _ := vals[0].(int64)
	return held == 1, nil
}

// Release drops one nesting level: every call runs the decrement-or-delete
// script so the Redis-side count matches the local count, and the key dies
// on the last level. Releasing after the lease lapsed is not an error: the
// key is simply not ours any more.
func (l *Locker) Release(ctx context.Context) error {
	l.mu.Lock()
	if len(l.acqs) == 0 {
		l.mu.Unlock()
		return errors.New("redis: lock " + l.key + ": release without hold")
	}
	outer := len(l.acqs) == 1
	l.acqs = l.acqs[1:]
	ttl := l.ttl
	l.mu.Unlock()

	// Every level goes through the script, not just the last: nested Lock
	// armed the count once per level, so Release must drop it once per
	// level. Popping locally and only calling Redis at the end left the
	// count at 1 above the local count, and the key outlived its holder.
	_, err := releaseScript.RunInt(ctx, l.c, []string{storageKey(l.key), storageKey(l.key) + ":queue"}, l.token, strconv.FormatInt(ttl.Milliseconds(), 10))
	if err != nil {
		return fmt.Errorf("redis: unlock %s: %w", l.key, err)
	}
	if outer {
		l.stopWatchdog()
	}
	return nil
}

// Renew extends the lease now; the watchdog already does this when asked.
func (l *Locker) Renew(ctx context.Context) error {
	l.mu.Lock()
	ttl := l.ttl
	l.mu.Unlock()
	n, err := renewScript.RunInt(ctx, l.c, []string{storageKey(l.key), storageKey(l.key) + ":queue"}, l.token, strconv.FormatInt(ttl.Milliseconds(), 10))
	if err != nil {
		return fmt.Errorf("redis: renew %s: %w", l.key, err)
	}
	if n != 1 {
		l.markLost()
		return ErrLockLost
	}
	return nil
}

// Held reports how many nesting levels this client holds.
func (l *Locker) Held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.acqs)
}

// Lost is closed when the held lock was lost (ErrLockLost); nil while
// healthy.
func (l *Locker) Lost() <-chan struct{} {
	return l.lost
}

func (l *Locker) checkHealthy() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastErr
}

func (l *Locker) startWatchdog(p lockParams) {
	interval := p.watchdog / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	l.mu.Lock()
	if l.watchStop != nil {
		l.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	l.watchStop = stop
	l.mu.Unlock()

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if !l.renewAll(context.Background()) {
					return
				}
			}
		}
	}()
}

// stopWatchdog ends the watchdog goroutine, if any. It is the only closer of
// watchStop, so a Release racing the watchdog's own exit cannot double-close.
func (l *Locker) stopWatchdog() {
	l.mu.Lock()
	stop := l.watchStop
	l.watchStop = nil
	l.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// renewAll renews the lock key and the waiter queue. On failure it marks
// the locker lost. Returns false when the watchdog should stop.
func (l *Locker) renewAll(ctx context.Context) bool {
	l.mu.Lock()
	ttl := l.ttl
	held := len(l.acqs) > 0
	l.mu.Unlock()
	if ttl <= 0 {
		return true
	}
	ttlMs := strconv.FormatInt(ttl.Milliseconds(), 10)
	sk := storageKey(l.key)
	skq := sk + ":queue"

	n, err := renewScript.RunInt(ctx, l.c, []string{sk, skq}, l.token, ttlMs)
	if err != nil {
		return true // transient: the next tick retries while the lease lasts
	}
	if n != 1 && held {
		// A release can race a tick: only mark lost when this client still
		// believes it holds the lock, so a clean Release never reads as a
		// loss.
		l.markLost()
		return false
	}
	return true
}

// markLost records that the lease is gone. It is idempotent so the watchdog
// and a manual Renew can both report the same loss without double-closing.
func (l *Locker) markLost() {
	l.mu.Lock()
	l.lastErr = ErrLockLost
	l.mu.Unlock()
	l.lostOnce.Do(func() { close(l.lost) })
}
