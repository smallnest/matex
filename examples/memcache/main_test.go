package main

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

	"github.com/smallnest/matex/pkg/core/memcache"
)

// Memcached has no ping, so the demo only needs a server that speaks the
// text protocol. This in-process stand-in covers gets/set/touch/delete,
// which is exactly what the demo calls, so the example runs offline.

func openClient(t *testing.T) *memcache.Client {
	t.Helper()
	return memcache.Open(memcache.Config{Addrs: []string{fakeMemcached(t)}, Timeout: time.Second, MaxIdle: 8})
}

func TestSetGetTouchDelete(t *testing.T) {
	c := openClient(t)
	key := fmt.Sprintf("example:session:%d", time.Now().UnixNano())
	want := session{UserID: 42, Token: "tk-42", At: time.Now().UTC().Truncate(time.Second)}

	if err := c.SetJSON(key, want, time.Minute); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, ok, err := memcache.GetJSON[session](c, key)
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.UserID != 42 || got.Token != "tk-42" || !got.At.Equal(want.At) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	// A miss is (zero, false, nil), never an error.
	if _, ok, err := memcache.GetJSON[session](c, key+":absent"); ok || err != nil {
		t.Fatalf("miss: ok=%v err=%v", ok, err)
	}

	if err := c.Touch(key, 2*time.Minute); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if _, ok, err := memcache.GetJSON[session](c, key); err != nil || !ok {
		t.Fatalf("after touch: ok=%v err=%v", ok, err)
	}

	if err := c.Delete(key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, err := memcache.GetJSON[session](c, key); ok || err != nil {
		t.Fatalf("after delete: ok=%v err=%v", ok, err)
	}
	if err := c.Delete(key); err != nil {
		t.Fatalf("deleting a missing key: %v", err)
	}
}

// fakeMemcached starts a text-protocol server on an ephemeral port and
// returns its address.
func fakeMemcached(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &store{items: map[string][]byte{}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return ln.Addr().String()
}

type store struct {
	mu    sync.Mutex
	items map[string][]byte
}

func (s *store) serve(c net.Conn) {
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
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "get", "gets":
			s.mu.Lock()
			hits := make(map[string][]byte, len(f)-1)
			for _, k := range f[1:] {
				if v, ok := s.items[k]; ok {
					hits[k] = v
				}
			}
			s.mu.Unlock()
			for k, v := range hits {
				if err := reply("VALUE %s 0 %d\r\n%s\r\n", k, len(v), v); err != nil {
					return
				}
			}
			err = reply("END\r\n")
		case "set":
			n, _ := strconv.Atoi(f[4])
			buf := make([]byte, n+2)
			if _, err := io.ReadFull(r, buf); err != nil {
				return
			}
			s.mu.Lock()
			s.items[f[1]] = buf[:n]
			s.mu.Unlock()
			err = reply("STORED\r\n")
		case "touch":
			s.mu.Lock()
			_, ok := s.items[f[1]]
			s.mu.Unlock()
			if ok {
				err = reply("TOUCHED\r\n")
			} else {
				err = reply("NOT_FOUND\r\n")
			}
		case "delete":
			s.mu.Lock()
			_, ok := s.items[f[1]]
			delete(s.items, f[1])
			s.mu.Unlock()
			if ok {
				err = reply("DELETED\r\n")
			} else {
				err = reply("NOT_FOUND\r\n")
			}
		default:
			err = reply("ERROR\r\n")
		}
		if err != nil {
			return
		}
	}
}
