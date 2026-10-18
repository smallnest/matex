// Package feature is the service's feature-flag switchboard: named boolean
// flags that turn a code path on or off per deployment without a rebuild.
//
//	func init() {
//		feature.Register(feature.Flag{Name: "new-checkout", Default: false})
//	}
//
//	if feature.Enabled("new-checkout") {
//		return s.checkoutV2(ctx, in)
//	}
//	return s.checkoutV1(ctx, in)
//
// Values are resolved per call, highest priority first:
//
//	runtime   Set(name, true)             — an operator switch, lasts this process
//	env       FEATURE_NEW_CHECKOUT=true   — per-deployment override
//	config    feature: {new-checkout: on} — in config.yaml, loaded by verticle
//	registry  the Flag's Default            — unknown flags are off
//
// The package deliberately does not persist or propagate flags: changes take
// effect in one process. A flag service is a deployment concern; this is the
// in-process seam that any such system can drive through Set.
package feature

import (
	"errors"
	"os"
	"strings"
	"sync"
)

// Flag declares a feature flag. Default applies when nothing overrides it.
type Flag struct {
	Name    string
	Default bool
}

var (
	mu      sync.RWMutex
	flags   = map[string]bool{} // Default values, from Register
	runtime = map[string]bool{} // from Set
	config  = map[string]bool{} // from Configure
)

// Register declares flags and their defaults. Registering the same name again
// updates its default and keeps any values already set.
func Register(fs ...Flag) {
	mu.Lock()
	defer mu.Unlock()
	for _, f := range fs {
		flags[f.Name] = f.Default
	}
}

// Set flips a flag at runtime. The strongest override; Set is the hook a
// control plane or admin endpoint calls.
func Set(name string, on bool) {
	mu.Lock()
	defer mu.Unlock()
	runtime[name] = on
}

// Configure replaces the values loaded from configuration. verticle.Run
// calls it once at startup with the framework's `feature:` section.
func Configure(values map[string]bool) {
	mu.Lock()
	defer mu.Unlock()
	config = make(map[string]bool, len(values))
	for k, v := range values {
		config[strings.ToLower(k)] = v
	}
}

// Enabled reports whether the flag is on under the current overrides. A
// flag nobody registered is off unless it was set explicitly by name.
func Enabled(name string) bool {
	if on, ok := lookup(name); ok {
		return on
	}
	return false
}

// Enabled reports whether the flag is on under the current overrides. A
// Flag value carries its own default, so it answers even before (or
// without) Register.
func (f Flag) Enabled() bool {
	if on, ok := lookup(f.Name); ok {
		return on
	}
	// lookup misses only when nobody registered or set it, so the Flag's
	// own default is the answer.
	return f.Default
}

// lookup resolves name along the priority chain; ok is false when nothing
// knows the flag.
func lookup(name string) (on, ok bool) {
	name = strings.ToLower(name)
	mu.RLock()
	defer mu.RUnlock()
	if v, ok := runtime[name]; ok {
		return v, true
	}
	// Read the environment on every call, not at Configure time: tests use
	// t.Setenv, and an operator may export a flag before exec.
	if v, ok := os.LookupEnv(envName(name)); ok {
		if b, err := parseBool(v); err == nil {
			return b, true
		}
	}
	if v, ok := config[name]; ok {
		return v, true
	}
	v, ok := flags[name]
	return v, ok
}

// envName maps a flag name to its environment variable: FEATURE_<NAME>, upper-
// cased with every non-alphanumeric character replaced by "_".
func envName(name string) string {
	var b strings.Builder
	b.WriteString("FEATURE_")
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// parseBool accepts the spellings an operator is likely to type, so a value
// like "yes" is not silently ignored.
func parseBool(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "on", "yes", "y", "enabled":
		return true, nil
	case "0", "false", "off", "no", "n", "disabled":
		return false, nil
	}
	return false, errors.New("feature: value is not a boolean")
}

// reset forgets everything; tests use it to isolate cases.
func reset() {
	mu.Lock()
	defer mu.Unlock()
	flags = map[string]bool{}
	runtime = map[string]bool{}
	config = map[string]bool{}
}
