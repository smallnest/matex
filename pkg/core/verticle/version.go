package verticle

import (
	"context"
	"net/http"
	"runtime"
	"runtime/debug"

	"github.com/smallnest/matex/pkg/core/httpx"
)

// VersionInfo is what GET /version reports. It answers the first question of
// any incident — which build is actually running — without shelling into the
// pod.
type VersionInfo struct {
	Service   string `json:"service"`
	GoVersion string `json:"go_version"`
	Module    string `json:"module,omitempty"`
	Version   string `json:"version,omitempty"`
	// Revision and Modified come from the VCS stamp the Go toolchain embeds
	// in the binary.
	Revision  string `json:"vcs_revision,omitempty"`
	Modified  bool   `json:"vcs_modified,omitempty"`
	BuildTime string `json:"build_time,omitempty"`
}

// versionHandler reports build metadata. It is read once: none of it can
// change while the process runs.
func versionHandler(service string) httpx.HandlerFunc {
	info := readVersion(service)
	return func(context.Context, *http.Request) (any, error) { return info, nil }
}

func readVersion(service string) VersionInfo {
	info := VersionInfo{Service: service, GoVersion: runtime.Version()}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	info.Module = bi.Main.Path
	info.Version = bi.Main.Version
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Revision = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		case "vcs.time":
			info.BuildTime = s.Value
		}
	}
	return info
}
