package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smallnest/matex/pkg/core/auth"
	"github.com/smallnest/matex/pkg/core/httpx"
)

const testSecret = "test-secret-for-authdemo"

// newTestHandler builds the real routes over a real verifier, so the tests
// drive the whole pipeline (middleware → envelope → handler) with no port
// and no config file.
func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	v, err := auth.New(auth.Config{Issuer: "matex-authdemo", Secret: testSecret})
	if err != nil {
		t.Fatalf("new verifier: %v", err)
	}
	srv := httpx.New(httpx.Config{Timeout: time.Second, MaxBody: 1 << 20})
	svc := &authDemo{verifier: v}
	if err := svc.BuildRouter(srv); err != nil {
		t.Fatalf("build router: %v", err)
	}
	return srv.Handler()
}

func do(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// login exchanges credentials for a token and returns it.
func login(t *testing.T, h http.Handler, user, pass string) string {
	t.Helper()
	rec := do(h, http.MethodPost, "/api/v1/login",
		`{"username":"`+user+`","password":"`+pass+`"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login: code %d body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Token == "" {
		t.Fatal("login returned no token")
	}
	return env.Data.Token
}

func TestLogin(t *testing.T) {
	h := newTestHandler(t)

	t.Run("valid credentials", func(t *testing.T) {
		if login(t, h, "bob", "bob-pw") == "" {
			t.Fatal("no token")
		}
	})

	t.Run("wrong password", func(t *testing.T) {
		rec := do(h, http.MethodPost, "/api/v1/login", `{"username":"bob","password":"nope"}`, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("code %d", rec.Code)
		}
	})

	t.Run("unknown user looks the same as a wrong password", func(t *testing.T) {
		rec := do(h, http.MethodPost, "/api/v1/login", `{"username":"nobody","password":"x"}`, "")
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid credentials") {
			t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
		}
	})
}

func TestProtectedRoutes(t *testing.T) {
	h := newTestHandler(t)
	alice := login(t, h, "alice", "alice-pw")
	bob := login(t, h, "bob", "bob-pw")

	cases := []struct {
		name       string
		path       string
		token      string
		wantStatus int
		wantBody   string
	}{
		{"profile without a token", "/api/v1/profile", "", http.StatusUnauthorized, `"code":40101`},
		{"profile with a token", "/api/v1/profile", alice, http.StatusOK, `"subject":"alice"`},
		{"admin route as a plain user", "/api/v1/admin/audit", alice, http.StatusForbidden, `"code":40301`},
		{"admin route as an admin", "/api/v1/admin/audit", bob, http.StatusOK, `"requested_by":"bob"`},
		{"tampered token", "/api/v1/profile", alice + "x", http.StatusUnauthorized, `"code":40101`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(h, http.MethodGet, tc.path, "", tc.token)
			if rec.Code != tc.wantStatus {
				t.Fatalf("code %d body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Fatalf("body %s, want %s", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestPublicRouteStaysOpen: /login is registered before any Use, so neither
// middleware sees it.
func TestPublicRouteStaysOpen(t *testing.T) {
	h := newTestHandler(t)
	rec := do(h, http.MethodPost, "/api/v1/login", `{"username":"alice","password":"alice-pw"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login must not require a token: code %d", rec.Code)
	}
}

// TestProfileExposesScopes proves the claims survive the round trip.
func TestProfileExposesScopes(t *testing.T) {
	h := newTestHandler(t)
	bob := login(t, h, "bob", "bob-pw")

	rec := do(h, http.MethodGet, "/api/v1/profile", "", bob)
	body := rec.Body.String()
	for _, want := range []string{`"audit:read"`, `"admin"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("profile body %s missing %s", body, want)
		}
	}
}
