package memcache

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A tiny in-process memcached speaking the text protocol, so the client is
// tested with no Docker. It stores ASCII items with their expiration
// seconds (-1 = never) and answers get/set/touch/delete.

type fakeMemcached struct {
	ln   net.Listener
	mu   sync.Mutex
	item map[string]fakeItem
}

type fakeItem struct {
	key    string
	value  []byte
	expiry int32
}

func newFakeMemcached(t *testing.T) *fakeMemcached {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeMemcached{ln: ln, item: map[string]fakeItem{}}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeMemcached) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	reply := func(format string, a ...any) error {
		_, err := fmt.Fprintf(c, format, a...)
		return err
	}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		switch parts[0] {
		case "get", "gets":
			f.mu.Lock()
			items := make([]fakeItem, 0, len(parts)-1)
			for _, k := range parts[1:] {
				if it, ok := f.item[k]; ok {
					items = append(items, it)
				}
			}
			f.mu.Unlock()
			// Reply outside the lock: a blocked write must not stall writers.
			for _, it := range items {
				if err := reply("VALUE %s 0 %d\r\n%s\r\n", it.key, len(it.value), it.value); err != nil {
					return
				}
			}
			err = reply("END\r\n")
		case "set":
			n, _ := strconv.Atoi(parts[4])
			exp, _ := strconv.ParseInt(parts[3], 10, 32)
			body := make([]byte, n+2)
			if _, err := io.ReadFull(r, body); err != nil {
				return
			}
			f.mu.Lock()
			f.item[parts[1]] = fakeItem{key: parts[1], value: body[:n], expiry: int32(exp)}
			f.mu.Unlock()
			err = reply("STORED\r\n")
		case "touch":
			exp, _ := strconv.ParseInt(parts[2], 10, 32)
			f.mu.Lock()
			it, ok := f.item[parts[1]]
			if ok {
				it.expiry = int32(exp)
				f.item[parts[1]] = it
			}
			f.mu.Unlock()
			if ok {
				err = reply("TOUCHED\r\n")
			} else {
				err = reply("NOT_FOUND\r\n")
			}
		case "delete":
			f.mu.Lock()
			_, ok := f.item[parts[1]]
			delete(f.item, parts[1])
			f.mu.Unlock()
			if ok {
				err = reply("DELETED\r\n")
			} else {
				err = reply("NOT_FOUND\r\n")
			}
		}
		if err != nil {
			return
		}
	}
}

func (f *fakeMemcached) open() *Client {
	return Open(Config{Addrs: []string{f.ln.Addr().String()}, Timeout: 500 * time.Millisecond})
}

func TestSetGetJSONRoundTrip(t *testing.T) {
	mc := newFakeMemcached(t).open()

	if err := mc.SetJSON("user:1", map[string]any{"id": 1.0, "name": "ann"}, time.Minute); err != nil {
		t.Fatalf("SetJSON: %v", err)
	}
	got, ok, err := GetJSON[map[string]any](mc, "user:1")
	if err != nil || !ok {
		t.Fatalf("GetJSON: ok=%v err=%v", ok, err)
	}
	if got["name"] != "ann" {
		t.Fatalf("got %v", got)
	}
}

func TestGetJSONMissAndCorrupt(t *testing.T) {
	f := newFakeMemcached(t)
	mc := f.open()

	_, ok, err := GetJSON[string](mc, "absent")
	if ok || err != nil {
		t.Fatalf("miss: ok=%v err=%v, want (false, nil)", ok, err)
	}

	f.mu.Lock()
	f.item["bad"] = fakeItem{key: "bad", value: []byte("not json"), expiry: -1}
	f.mu.Unlock()
	_, ok, err = GetJSON[string](mc, "bad")
	if ok || err == nil {
		t.Fatalf("corrupt value: ok=%v err=%v, want an error", ok, err)
	}
}

func TestSetJSONStoresTTLInSeconds(t *testing.T) {
	f := newFakeMemcached(t)
	if err := f.open().SetJSON("k", 1, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := f.item["k"].expiry; got != 90 {
		t.Fatalf("expiry = %d s, want 90", got)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	f := newFakeMemcached(t)
	mc := f.open()

	if err := mc.SetJSON("k", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := mc.Delete("k"); err != nil {
			t.Fatalf("Delete #%d: %v", i+1, err)
		}
	}
	if _, ok, _ := GetJSON[int](mc, "k"); ok {
		t.Fatal("key survived Delete")
	}
}

func TestTouchExtendsTTL(t *testing.T) {
	f := newFakeMemcached(t)
	mc := f.open()

	if err := mc.SetJSON("k", 1, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := mc.Touch("k", 300*time.Second); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	f.mu.Lock()
	got := f.item["k"].expiry
	f.mu.Unlock()
	if got != 300 {
		t.Fatalf("expiry after touch = %d s, want 300", got)
	}
}

func TestUnreachableServerIsAnError(t *testing.T) {
	// Grab a port and close it: nothing listens there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	mc := Open(Config{Addrs: []string{addr}, Timeout: 200 * time.Millisecond})
	if err := mc.SetJSON("k", 1, time.Minute); err == nil {
		t.Fatal("SetJSON against a dead server must fail")
	}
	if _, ok, err := GetJSON[int](mc, "k"); ok || err == nil {
		t.Fatal("GetJSON against a dead server must report an error, not a miss")
	}
}

func TestSetJSONRejectsUnencodableValue(t *testing.T) {
	mc := newFakeMemcached(t).open()
	if err := mc.SetJSON("k", make(chan int), time.Minute); err == nil {
		t.Fatal("SetJSON must fail on a value json cannot encode")
	}
}

func TestSubSecondTTLRoundsUpInsteadOfNeverExpiring(t *testing.T) {
	f := newFakeMemcached(t)
	mc := f.open()

	if err := mc.SetJSON("k", 1, 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got := f.item["k"].expiry
	f.mu.Unlock()
	if got != 1 {
		t.Fatalf("expiry = %d s, want 1 (truncating to 0 would never expire)", got)
	}

	if err := mc.Touch("k", 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	got = f.item["k"].expiry
	f.mu.Unlock()
	if got != 1 {
		t.Fatalf("touch expiry = %d s, want 1", got)
	}
}
