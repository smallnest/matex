// Command feature demonstrates pkg/core/feature: named on/off switches read
// with one call, overridable per deployment without a rebuild.
//
// Resolution order, strongest first: runtime feature.Set, the
// FEATURE_<NAME> environment variable, the `feature:` config section, the
// flag's registered default.
//
// Run:
//
//	go run ./examples/feature
//
//	curl -s localhost:8085/api/v1/checkout            # the flag decides the path
//	curl -s -X POST "localhost:8085/api/v1/flags/beta-search?on=true"   # runtime flip
//	curl -s localhost:8085/api/v1/flags                # every flag and its value
//
//	FEATURE_BETA_SEARCH=on go run ./examples/feature  # env wins over config
//
//	go test ./examples/feature
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/smallnest/matex/pkg/core/errs"
	"github.com/smallnest/matex/pkg/core/feature"
	"github.com/smallnest/matex/pkg/core/httpx"
	"github.com/smallnest/matex/pkg/core/verticle"
)

// The flags this service owns, with their defaults. verticle loads the
// `feature:` section before Setup; Register only fills in the defaults.
var (
	flagNewCheckout = feature.Flag{Name: "new-checkout", Default: false}
	flagBetaSearch  = feature.Flag{Name: "beta-search", Default: false}
)

func init() {
	feature.Register(flagNewCheckout, flagBetaSearch)
}

type featureService struct{}

func (s *featureService) Name() string { return "feature" }

func (s *featureService) Setup(context.Context, *verticle.Env) error { return nil }

func (s *featureService) BuildRouter(srv *httpx.Server) error {
	srv.Handle("GET", "/api/v1/checkout", s.checkout)
	srv.Handle("GET", "/api/v1/flags", s.list)
	srv.Handle("POST", "/api/v1/flags/{name}", s.set)
	return nil
}

// checkout picks its implementation per request, so a flip takes effect on
// the next call — no restart, no stale copy in a struct field.
func (s *featureService) checkout(context.Context, *http.Request) (any, error) {
	if flagNewCheckout.Enabled() {
		return map[string]string{"path": "checkout-v2", "note": "one-page flow"}, nil
	}
	return map[string]string{"path": "checkout-v1", "note": "legacy three-step flow"}, nil
}

func (s *featureService) list(_ context.Context, _ *http.Request) (any, error) {
	return map[string]bool{
		flagNewCheckout.Name: flagNewCheckout.Enabled(),
		flagBetaSearch.Name:  flagBetaSearch.Enabled(),
	}, nil
}

// set is the operator switch. Real services guard it (auth middleware,
// admin-only); it exists here to show the runtime override.
func (s *featureService) set(_ context.Context, r *http.Request) (any, error) {
	name := r.PathValue("name")
	switch r.URL.Query().Get("on") {
	case "true", "1", "on":
		feature.Set(name, true)
	case "false", "0", "off":
		feature.Set(name, false)
	default:
		return nil, errs.Invalid(40001, "on must be true or false")
	}
	return map[string]any{"name": name, "enabled": feature.Enabled(name)}, nil
}

func main() {
	conf := flag.String("conf", "examples/feature/config.yaml", "config file")
	flag.Parse()

	if err := verticle.Run(context.Background(), &featureService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
