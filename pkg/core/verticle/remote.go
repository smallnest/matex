package verticle

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/smallnest/matex/pkg/core/config"
	"github.com/smallnest/matex/pkg/core/nacos"
	"github.com/smallnest/matex/pkg/core/obs"
)

// The config center gives two things with different lifetimes: the first
// generation (needed before anything is built, so Run waits for it) and
// every later generation (applied while serving). A ConfigSource is called
// exactly once: its Watch delivers the first generation itself, then keeps
// calling back.

// cfgNacos reads just the `nacos:` section out of the raw file.
type cfgNacos struct {
	Nacos nacos.Config `json:"nacos" optional:""`
}

// remoteGen is one generation of the center's answer.
type remoteGen struct {
	fw  map[string]any // framework keys
	svc map[string]any // service section; nil: it stays in the file
}

// remoteTimeout prefers the source's own timeout (nacos.Config.Timeout) and
// falls back to a fixed budget for other implementations.
func remoteTimeout(src ConfigSource) time.Duration {
	type timed interface{ Timeout() time.Duration }
	if t, ok := src.(timed); ok {
		if d := t.Timeout(); d > 0 {
			return d
		}
	}
	return 10 * time.Second
}

// seed starts the source and waits for its first generation, bounded by the
// source's timeout. Booting on a stale snapshot would run the service on
// config nobody chose, so a timeout fails the boot.
//
// The source's own context is without cancel: canceling the first-fetch
// budget when the first generation arrives would also cancel the
// subscription, and the source would stop watching. Run's ctx is attached
// once the app is running.
func seed(src ConfigSource) (*remoteGen, chan remoteGen, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), remoteTimeout(src))

	first := make(chan remoteGen, 1)
	gens := make(chan remoteGen, 16)
	errCh := make(chan error, 1)
	go func() {
		errCh <- src.Watch(ctx, func(fw, svc map[string]any) {
			g := remoteGen{fw: orEmpty(fw), svc: svc}
			select {
			case first <- g:
			default:
			}
			select {
			case gens <- g:
			default: // a newer generation supersedes a pending one
			}
		})
	}()
	select {
	case g := <-first:
		return &g, gens, cancel, nil
	case err := <-errCh:
		cancel()
		if err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, nil, fmt.Errorf("config center delivered nothing")
	case <-ctx.Done():
		cancel()
		return nil, nil, nil, fmt.Errorf("first fetch did not finish in time: %w", ctx.Err())
	}
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// applyRemoteLive waits until the source is subscribed, then applies every
// later generation. The section goes live, exactly like a file edit;
// framework keys are reported but not applied, because pools, listeners and
// tracing were built at boot.
func applyRemoteLive(ctx context.Context, gens <-chan remoteGen, boot *remoteGen, svc Service, env *Env) {
	prev := boot
	for {
		select {
		case <-ctx.Done():
			return
		case g := <-gens:
			if reflect.DeepEqual(prev, &g) {
				continue
			}
			if !reflect.DeepEqual(prev.fw, g.fw) {
				obs.Warn(ctx, "framework config changed on the config center; restart or roll out to apply it (the service section applies live)")
			}
			if g.svc != nil && !reflect.DeepEqual(prev.svc, g.svc) {
				// Same order as the file watcher: the section is swapped
				// before the callback, so env.DecodeService inside it sees
				// the new values.
				env.setServiceSection(g.svc)
				if cw, ok := svc.(ConfigWatcher); ok {
					cw.OnServiceConfigChange(g.svc)
				}
				obs.Info(ctx, "service config reloaded", "source", "config center")
			}
			prev = &g
		}
	}
}

// remoteOverlay merges the center's framework keys over the file, keeping
// the service's own section out of the framework half.
func remoteOverlay(file map[string]any, g *remoteGen, svcKey string) map[string]any {
	fw := g.fw
	if _, isSection := fw[svcKey].(map[string]any); isSection {
		clean := make(map[string]any, len(fw))
		for k, v := range fw {
			if k != svcKey {
				clean[k] = v
			}
		}
		fw = clean
	}
	return config.Merge(file, fw)
}
