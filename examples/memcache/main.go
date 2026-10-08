// Command memcache demonstrates pkg/core/memcache: JSON values with a TTL.
//
// Memcached has no ping, so Open never fails on a dead server — the first
// command does. Start one with `make dev` first.
//
// Run:
//
//	make dev
//	go run ./examples/memcache
//	go run ./examples/memcache -key other:key -ttl 5s
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/smallnest/matex/pkg/core/memcache"
)

type session struct {
	UserID int       `json:"user_id"`
	Token  string    `json:"token"`
	At     time.Time `json:"at"`
}

func main() {
	addrs := flag.String("addrs", "localhost:11211", "comma-separated memcached addresses")
	key := flag.String("key", "example:session:42", "cache key")
	ttl := flag.Duration("ttl", 60*time.Second, "TTL (memcached granularity is 1s)")
	flag.Parse()

	c := memcache.Open(memcache.Config{
		Addrs:   strings.Split(*addrs, ","),
		Timeout: time.Second,
		MaxIdle: 8,
	})

	want := session{UserID: 42, Token: "tk-42", At: time.Now().UTC()}

	// 1. Write.
	if err := c.SetJSON(*key, want, *ttl); err != nil {
		fail(err)
	}
	fmt.Printf("set      %s (ttl=%s)\n", *key, *ttl)

	// 2. Read back.
	got, ok, err := memcache.GetJSON[session](c, *key)
	if err != nil {
		fail(err)
	}
	if !ok {
		fail(fmt.Errorf("expected a hit for %s", *key))
	}
	fmt.Printf("get      hit  → user_id=%d token=%s at=%s\n",
		got.UserID, got.Token, got.At.Format(time.RFC3339))

	// 3. A miss is (zero, false, nil), never an error.
	if _, ok, err := memcache.GetJSON[session](c, *key+":absent"); err != nil || ok {
		fail(fmt.Errorf("expected a miss, got ok=%v err=%v", ok, err))
	}
	fmt.Println("get      miss → ok=false err=nil（miss 不报错）")

	// 4. Extend the TTL without rewriting the value.
	if err := c.Touch(*key, *ttl+time.Minute); err != nil {
		fail(err)
	}
	fmt.Printf("touch    %s (ttl=%s)\n", *key, *ttl+time.Minute)

	// 5. Delete, then confirm the miss.
	if err := c.Delete(*key); err != nil {
		fail(err)
	}
	if _, ok, err := memcache.GetJSON[session](c, *key); err != nil || ok {
		fail(fmt.Errorf("after delete, got ok=%v err=%v", ok, err))
	}
	fmt.Println("delete   → 再读是 miss（Delete 删不存在的 key 也不报错）")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "fatal:", err)
	fmt.Fprintln(os.Stderr, "hint : 需要 memcached（make dev），或用 -addrs 指定地址")
	os.Exit(1)
}
