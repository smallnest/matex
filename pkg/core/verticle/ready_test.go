package verticle

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/smallnest/matex/pkg/core/memcache"

	"github.com/smallnest/matex/pkg/core/dbtest"
	"github.com/smallnest/matex/pkg/core/redistest"
)

// readyz must notice every configured dependency: a pod whose redis or
// memcache died should leave the load-balancer pool, not keep serving.

func TestReadyChecksEveryConfiguredDependency(t *testing.T) {
	svc := &struct{ Service }{}

	t.Run("nothing configured", func(t *testing.T) {
		if err := makeReady(svc, &Env{})(t.Context()); err != nil {
			t.Fatalf("ready without dependencies: %v", err)
		}
	})

	t.Run("db", func(t *testing.T) {
		err := makeReady(svc, &Env{db: dbtest.Start(t)})(t.Context())
		if err != nil {
			t.Fatalf("ready with a live db: %v", err)
		}
	})

	t.Run("redis", func(t *testing.T) {
		err := makeReady(svc, &Env{redis: redistest.Start(t)})(t.Context())
		if err != nil {
			t.Fatalf("ready with a live redis: %v", err)
		}
	})

	t.Run("memcache", func(t *testing.T) {
		addr, closeServer := fakeMemcached(t)
		mc := memcache.Open(memcache.Config{Addrs: []string{addr}})
		if err := makeReady(svc, &Env{memcache: mc})(t.Context()); err != nil {
			t.Fatalf("ready with a live memcache: %v", err)
		}
		closeServer()
		if err := makeReady(svc, &Env{memcache: mc})(t.Context()); err == nil {
			t.Fatal("ready must fail once memcache is unreachable")
		}
	})
}

// fakeMemcached answers every get with END (a miss): enough for readyz,
// which only needs the server to answer. closeFn resets live connections
// and stops the listener, the way a dying server does.
func fakeMemcached(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	quit := make(chan struct{})
	var conns sync.WaitGroup
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conn := c.(*net.TCPConn)
			conns.Go(func() {
				stop := make(chan struct{})
				defer close(stop)
				go func() {
					select {
					case <-quit:
						// A reset makes the next command fail even if the
						// client still holds the descriptor.
						_ = conn.SetLinger(0)
						_ = conn.Close()
					case <-stop:
					}
				}()
				defer func() { _ = conn.Close() }()
				r := bufio.NewReader(conn)
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					// gets is followed by a comma-separated key list; the
					// reply is still END-terminated.
					if strings.HasPrefix(line, "get") {
						_, _ = fmt.Fprint(conn, "END\r\n")
					}
				}
			})
		}
	})
	var once sync.Once
	return ln.Addr().String(), func() {
		once.Do(func() {
			close(quit)
			_ = ln.Close()
			conns.Wait() // every reset has landed before the caller probes
		})
		wg.Wait()
	}
}
