package httpx

import (
	"net/http"
	"net/http/pprof"
)

// mountPprof registers the standard library profiler under /debug/pprof/.
//
// These are raw handlers, not matex handlers: pprof writes binary profiles
// and plain text, so it must not pass through the JSON envelope. The
// profile list (heap, goroutine, allocs, block, mutex, threadcreate) is
// served by pprof.Index from the path suffix.
func mountPprof(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
}
