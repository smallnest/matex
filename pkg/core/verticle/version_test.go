package verticle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
)

func TestVersionHandlerReportsTheBuild(t *testing.T) {
	h := versionHandler("demo")

	data, err := h(context.Background(), httptest.NewRequest(http.MethodGet, "/version", nil))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	info, ok := data.(VersionInfo)
	if !ok {
		t.Fatalf("data = %T, want VersionInfo", data)
	}
	if info.Service != "demo" {
		t.Errorf("service = %q", info.Service)
	}
	if info.GoVersion != runtime.Version() {
		t.Errorf("go_version = %q, want %q", info.GoVersion, runtime.Version())
	}
	// The module path is always available for a built binary.
	if info.Module == "" {
		t.Error("module is empty")
	}
}

// TestVersionIsStatic: none of the reported values can change while the
// process runs, so it is read once rather than per request.
func TestVersionIsStatic(t *testing.T) {
	h := versionHandler("demo")
	first, _ := h(context.Background(), httptest.NewRequest(http.MethodGet, "/version", nil))
	second, _ := h(context.Background(), httptest.NewRequest(http.MethodGet, "/version", nil))
	if first != second {
		t.Fatalf("version changed between calls: %+v vs %+v", first, second)
	}
}
