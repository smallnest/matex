package feature

import "testing"

// Each test resets the registry: the package is process-global by design.

func TestDefaultWhenNothingOverrides(t *testing.T) {
	reset()
	Register(Flag{Name: "checkout-v2", Default: true}, Flag{Name: "beta", Default: false})
	if !Enabled("checkout-v2") {
		t.Fatal("the default true was not honored")
	}
	if Enabled("beta") {
		t.Fatal("the default false was not honored")
	}
	if Enabled("never-registered") {
		t.Fatal("an unknown flag must be off")
	}
	if !(Flag{Name: "unregistered", Default: true}).Enabled() {
		t.Fatal("Flag.Enabled must use the flag's own default for an unknown name")
	}
}

func TestConfigOverridesDefault(t *testing.T) {
	reset()
	Register(Flag{Name: "beta", Default: false})
	Configure(map[string]bool{"Beta": true}) // keys are case-insensitive
	if !Enabled("beta") {
		t.Fatal("configuration did not override the default")
	}
	// Configure replaces the previous set wholesale.
	Configure(map[string]bool{})
	if Enabled("beta") {
		t.Fatal("Configure must replace, not merge")
	}
}

func TestEnvOverridesConfigAndDefault(t *testing.T) {
	reset()
	Register(Flag{Name: "checkout-v2", Default: false})
	Configure(map[string]bool{"checkout-v2": false})
	t.Setenv("FEATURE_CHECKOUT_V2", "on")
	if !Enabled("checkout-v2") {
		t.Fatal("the environment must override configuration")
	}
	// A flag no one registered can still be switched by environment.
	t.Setenv("FEATURE_EXPERIMENT", "yes")
	if !Enabled("experiment") {
		t.Fatal("an env-only flag was not honored")
	}
	// An unparseable value falls through to the next source.
	t.Setenv("FEATURE_CHECKOUT_V2", "maybe")
	if Enabled("checkout-v2") {
		t.Fatal("an invalid env value must be ignored, not treated as true")
	}
}

func TestRuntimeSetWinsOverEverything(t *testing.T) {
	reset()
	Register(Flag{Name: "kill-switch", Default: false})
	Configure(map[string]bool{"kill-switch": false})
	t.Setenv("FEATURE_KILL_SWITCH", "true")
	Set("kill-switch", false)
	if Enabled("kill-switch") {
		t.Fatal("Set must override env and config")
	}
	Set("kill-switch", true)
	if !Enabled("kill-switch") {
		t.Fatal("Set(true) was not honored")
	}
}

func TestRegisterAgainKeepsOverrides(t *testing.T) {
	reset()
	Register(Flag{Name: "x", Default: false})
	Set("x", true)
	Register(Flag{Name: "x", Default: false})
	if !Enabled("x") {
		t.Fatal("re-registering a flag must not clear its runtime value")
	}
}

func TestEnvName(t *testing.T) {
	for name, want := range map[string]string{
		"new-checkout": "FEATURE_NEW_CHECKOUT",
		"a.b c":        "FEATURE_A_B_C",
		"plain":        "FEATURE_PLAIN",
	} {
		if got := envName(name); got != want {
			t.Errorf("envName(%q) = %q, want %q", name, got, want)
		}
	}
}
