package httpx

import (
	"net/http"
	"strings"
)

// Security defaults are on: a server that forgets to add headers is the
// common case, and a header that is wrong for one route can be relaxed on
// that route, while a missing one is invisible until an audit finds it.
// Between them the headers cost nothing per request.

// SecureHeaders adds the response headers that hold when the API is served
// to browsers: nosniff (no MIME guessing), a strict referrer policy, no
// framing (clickjacking), no browser features, and — under https — HSTS.
// The API answers JSON, so the content-security-policy is maximal:
// nothing may be loaded or executed. Registered as an outer middleware in
// New, before any UseOuter middleware, so it also covers /healthz, /readyz,
// /metrics, HandleRaw and error responses rendered by the guard.
func SecureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		// HSTS only over TLS: sent over plain http it is ignored at best,
		// and on a shared localhost port it would pin a browser to https
		// for a host that has none.
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// CORSPolicy allows browsers on the listed origins to call the API.
// With an empty list (the default) no CORS headers are sent: the API is
// same-origin, and a wildcard "*" would invite any page to read answers
// the browser still attaches the user's cookies to.
//
// Requests get the "Vary: Origin" header even when denied, so a cache
// never serves one origin's answer to another.
func CORSPolicy(origins ...string) OuterMiddleware {
	allowed := make(map[string]bool, len(origins))
	wildcard := false
	for _, o := range origins {
		switch o {
		case "*":
			wildcard = true
		case "":
		default:
			allowed[o] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			w.Header().Add("Vary", "Origin")
			if origin != "" && (wildcard || allowed[origin]) {
				if wildcard {
					// "*" cannot be combined with credentials, and this
					// server never reads cookies for auth.
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
				h := r.Header.Get("Access-Control-Request-Headers")
				w.Header().Set("Access-Control-Allow-Headers", h)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Max-Age", "600")
				w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
			} else {
				w.Header().Del("Access-Control-Allow-Credentials")
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
