// Helpers for remote config sources: pick the framework part out of a remote
// YAML body and extract a section from formats other than YAML.
package config

import (
	"fmt"
	"strings"
)

// RemoteYAML keeps only the framework keys of a remote YAML body. svcName is
// the lowercased Service.Name(): that section stays local (a file edit still
// hot-reloads it). A top-level scalar with the service's name is never read
// as that section. Keys outside the framework schema are reported as
// unknown, so a typo in the central config is visible at boot.
//
// fw is the framework config struct (a pointer). Pass nil when the caller
// reports unknown keys on its own; then only the section is removed.
//
// The whole body must parse: a broken central config fails the boot instead
// of silently falling back to half a file.
func RemoteYAML(body []byte, svcName string, fw any) (map[string]any, []string, error) {
	raw, err := parseRaw(body)
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		if k == svcName {
			continue
		}
		out[k] = v
	}
	if fw == nil {
		return out, nil, nil
	}
	return out, UnknownKeys(out, fw), nil
}

// Extract returns the section of a remote body as a raw map. The body may be
//
//   - YAML with several top-level keys: only a key named section can be
//     read as the section. Multi-section YAML without it, or the layout of a
//     whole framework file ("remote:"), fails — a body written for another
//     service is a deployment mistake, not an empty section;
//   - YAML holding only the section's own keys ("addr: ..."): the body is
//     the section;
//   - flat key=value lines (Nacos's own "properties" type). If keys are
//     prefixed with the section name ("app.addr=...") the section is
//     selected, otherwise the whole body is the section.
//
// A blank body is an error: a service silently configured from nothing is
// far harder to diagnose than a failed boot. YAML is the first choice — a
// properties body is recognised only when every line has a '='.
func Extract(body, section string) (map[string]any, error) {
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("config: remote config for section %q is empty", section)
	}
	if km, ok := parseKeyValue(body); ok {
		if err := checkBinary(section, km); err != nil {
			return nil, err
		}
		return extractFlat(km, section), nil
	}
	raw, err := parseRaw([]byte(body))
	if err != nil {
		return nil, err
	}
	_, hasRemote := raw["remote"].(map[string]any)
	sec, isSection := raw[section].(map[string]any)
	if isSection {
		return sec, nil
	}
	if hasRemote {
		return nil, fmt.Errorf("config: section %q not found in remote config", section)
	}
	// Otherwise the body is a doc of the section's own keys: "port: 8080",
	// or everything under one wrapper key. Decoding the extra level (a
	// wrapper, or a section of another name) fails with a key error, which
	// names the mistake.
	return raw, nil
}

// checkBinary refuses a properties body that went through a binary config
// type by mistake: such a body cannot be decoded, so failing loudly beats an
// empty section.
func checkBinary(section string, km map[string]string) error {
	if _, ok := km["dataId"]; ok {
		if _, ok := km["group"]; ok {
			return fmt.Errorf("config: remote config for section %q looks like a binary payload; set type to yaml or text", section)
		}
	}
	return nil
}

// extractFlat selects a prefixed section from a flat key=value body, or
// treats the whole body as the section.
func extractFlat(km map[string]string, section string) map[string]any {
	prefix := section + "."
	own := make(map[string]string, len(km))
	for k, v := range km {
		if strings.HasPrefix(k, prefix) {
			own[k] = v
		}
	}
	if len(own) == 0 {
		own = km
	}
	out := make(map[string]any, len(own))
	for k, v := range own {
		parts := strings.Split(strings.TrimPrefix(k, prefix), ".")
		cur := out
		for _, p := range parts[:len(parts)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[p] = next
			}
			cur = next
		}
		cur[parts[len(parts)-1]] = v
	}
	return out
}

// parseKeyValue reads "k=v" lines (Java properties style). ok is false as
// soon as one line is not a key=value pair, which is how a YAML body is told
// apart: "addr: \":8080\"" has its ':' before any '='. ISO-8859-1 input is
// taken through Latin-1, since Java properties are defined that way.
func parseKeyValue(body string) (map[string]string, bool) {
	out := make(map[string]string)
	lines := 0
	for _, line := range strings.Split(toUTF8(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		lines++
		i := strings.IndexAny(line, "=:")
		if i <= 0 {
			return nil, false
		}
		if j := strings.IndexByte(line, '='); j < 0 || j > i {
			return nil, false
		}
		val := strings.ReplaceAll(strings.TrimSpace(line[i+1:]), `\:`, ":")
		out[strings.TrimSpace(line[:i])] = val
	}
	return out, lines > 0
}

func toUTF8(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			r := make([]rune, 0, len(s))
			for j := 0; j < len(s); j++ {
				r = append(r, rune(s[j]))
			}
			return string(r)
		}
	}
	return s
}

// Merge returns dst overlaid by src, keys of src winning. Nested maps merge
// key by key, so a remote file can set one framework field without
// restating the section: a remote `redis: {addr: ...}` keeps the local
// `redis.password`. Slices and scalars replace wholesale. Neither input is
// modified.
func Merge(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := out[k].(map[string]any); ok {
				out[k] = Merge(dm, sm)
				continue
			}
		}
		out[k] = v
	}
	return out
}
