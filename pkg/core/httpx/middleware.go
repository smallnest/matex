package httpx

import (
	"net/http"
)

// Middleware wraps a HandlerFunc. It runs inside the framework wrapper,
// so it already sees the request-scoped context (trace id, per-request
// timeout) and can short-circuit by returning an error — the framework
// renders it as the usual envelope. That is the whole point: middleware
// never touches the ResponseWriter, so auth, rate limiting, idempotency
// and friends all stay in the same convention as ordinary handlers.
//
//	func RequireTenant(next httpx.HandlerFunc) httpx.HandlerFunc {
//		return func(ctx context.Context, r *http.Request) (any, error) {
//			id := r.Header.Get("X-Tenant-ID")
//			if id == "" {
//				return nil, errs.Invalid(40010, "missing tenant")
//			}
//			return next(WithTenantID(ctx, id), r)
//		}
//	}
//
// The first middleware passed to Use is the outermost: it runs first and
// its post-processing runs last.
type Middleware func(next HandlerFunc) HandlerFunc

// OuterMiddleware wraps the whole net/http stack, mux included. Reach for
// it only when a concern must touch the ResponseWriter itself (CORS
// headers, gzip, real client IP) rather than the envelope. One upside:
// because it sits outside the mux, it also covers /healthz, /readyz,
// /metrics and HandleRaw routes, which envelope middleware does not see.
type OuterMiddleware func(next http.Handler) http.Handler

// Use appends envelope middleware to the server. It guards every route
// registered after the call, so register public routes first, then Use,
// then the routes you want guarded:
//
//	func (s *Svc) BuildRouter(srv *httpx.Server) error {
//		srv.Handle("POST", "/api/v1/login", s.login) // public
//		srv.Use(ratelimit.Middleware(s.rl), auth.Middleware(s.auth))
//		srv.Handle("GET", "/api/v1/orders", s.list) // guarded
//		return nil
//	}
//
// Middleware accumulates: calling Use twice stacks the two groups, and
// like everywhere else the earlier group stays outermost.
func (s *Server) Use(mw ...Middleware) {
	s.mws = append(s.mws, mw...)
}

// UseOuter appends net/http-level middleware. Unlike Use it may be called
// at any time before serving: the handler chain is rebuilt on each call,
// keeping panic recovery outermost so a panicking middleware is still
// caught by the framework.
func (s *Server) UseOuter(mw ...OuterMiddleware) {
	s.outers = append(s.outers, mw...)
	s.rebuildHandler()
}

// apply folds the middleware around one route handler. Iterating backwards
// makes the first registered middleware the outermost.
func (s *Server) apply(h HandlerFunc) HandlerFunc {
	for i := len(s.mws) - 1; i >= 0; i-- {
		h = s.mws[i](h)
	}
	return h
}

// rebuildHandler re-wraps the mux: guard(outer[0](outer[1](…mux))).
func (s *Server) rebuildHandler() {
	var h http.Handler = s.mux
	for i := len(s.outers) - 1; i >= 0; i-- {
		h = s.outers[i](h)
	}
	s.http.Handler = s.guard(h)
}
