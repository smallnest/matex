// Package config provides YAML configuration loading with environment
// expansion, struct-tag defaults and per-field env overrides.
//
// # File syntax
//
// A config file is plain YAML. Before parsing, ${VAR} and ${VAR:default}
// are expanded from the environment:
//
//	db:
//	  dsn: ${DB_DSN:postgres://localhost/matex?sslmode=disable}
//
// # Struct tags
//
// Field names resolve from `json` tags, falling back to lowercased field
// names (all keys are matched case-insensitively):
//
//	default:"..."    used when the key is absent (or nil)
//	env:"VAR"        the environment variable overrides the file value
//	optional         absent without default leaves the zero value;
//	                 without it a missing key is an error
//
// # Supported types
//
// Basic kinds, time.Duration ("5s", or a number = milliseconds),
// []string ("a,b,c" via env/default), []int, map[string]T, nested
// structs and pointers to structs.
package config

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Load reads the YAML file at path and decodes it into v.
func Load(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := Parse(b, v); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}
	return nil
}

// LoadMap reads the YAML file at path into a key-lowercased raw map.
// It is the composition primitive used by the verticle package.
func LoadMap(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	m, err := parseRaw(b)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return m, nil
}

// Parse parses YAML bytes into v (same rules as Load).
func Parse(b []byte, v any) error {
	m, err := parseRaw(b)
	if err != nil {
		return err
	}
	return ParseMap(m, v)
}

// ParseMap decodes a key-lowercased raw map into v.
func ParseMap(m map[string]any, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("config: target must be a non-nil pointer to struct")
	}
	return decodeMap(m, rv.Elem(), "")
}

// parseRaw unmarshals YAML (after env expansion) into a lowercased map.
func parseRaw(b []byte) (map[string]any, error) {
	var raw map[string]any
	if err := yaml.Unmarshal([]byte(expandEnv(string(b))), &raw); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	return lowerKeys(raw), nil
}

var envRe = regexp.MustCompile(`\$\{(\w+)(?::([^}]*))?\}`)

// expandEnv expands ${VAR} and ${VAR:default} from the environment.
func expandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string {
		g := envRe.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(g[1]); ok {
			return v
		}
		return g[2] // default, possibly empty
	})
}

// lowerKeys lowercases map keys recursively (yaml is case-insensitive here).
func lowerKeys(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = lowerValue(v)
	}
	return out
}

func lowerValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return lowerKeys(x)
	case []any:
		for i, it := range x {
			x[i] = lowerValue(it)
		}
	}
	return v
}

var durationType = reflect.TypeFor[time.Duration]()

// decodeMap decodes m into the struct v. path is the key path for errors.
func decodeMap(m map[string]any, v reflect.Value, path string) error {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		key := fieldKey(f)
		fpath := join(path, key)
		fv := v.Field(i)

		// env override wins over the file value
		if envName, ok := f.Tag.Lookup("env"); ok {
			if s, present := os.LookupEnv(envName); present {
				if err := setString(fv, s, fpath); err != nil {
					return err
				}
				continue
			}
		}

		raw, ok := m[key]
		if !ok || raw == nil {
			if d, has := f.Tag.Lookup("default"); has {
				if err := setString(fv, d, fpath); err != nil {
					return err
				}
			} else if _, has := f.Tag.Lookup("optional"); !has {
				return fmt.Errorf("missing key %q", fpath)
			}
			continue
		}
		if err := decodeValue(raw, fv, fpath); err != nil {
			return err
		}
	}
	return nil
}

// fieldKey resolves the map key of a struct field.
func fieldKey(f reflect.StructField) string {
	if tag := f.Tag.Get("json"); tag != "" {
		if name, _, _ := strings.Cut(tag, ","); name != "" {
			return strings.ToLower(name)
		}
	}
	return strings.ToLower(f.Name)
}

func decodeValue(raw any, v reflect.Value, path string) error {
	if v.Type() == durationType {
		return setDuration(v, raw, path)
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		return decodeValue(raw, v.Elem(), path)
	case reflect.Struct:
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("key %q: expected map, got %T", path, raw)
		}
		return decodeMap(m, v, path)
	case reflect.Map:
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("key %q: expected map, got %T", path, raw)
		}
		out := reflect.MakeMapWithSize(v.Type(), len(m))
		for k, val := range m {
			ev := reflect.New(v.Type().Elem()).Elem()
			if err := decodeValue(val, ev, join(path, k)); err != nil {
				return err
			}
			kv := reflect.New(v.Type().Key()).Elem()
			if err := setString(kv, k, join(path, k)); err != nil {
				return err
			}
			out.SetMapIndex(kv, ev)
		}
		v.Set(out)
		return nil
	case reflect.Slice:
		items, ok := raw.([]any)
		if !ok {
			// a plain string (from env expansion) covers []string etc.
			return setString(v, toString(raw), path)
		}
		out := reflect.MakeSlice(v.Type(), len(items), len(items))
		for i, it := range items {
			if err := decodeValue(it, out.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		v.Set(out)
		return nil
	case reflect.String:
		v.SetString(toString(raw))
		return nil
	default:
		return setScalar(v, raw, path)
	}
}

// setScalar sets v from a raw YAML value (bool/int64/float64/string).
func setScalar(v reflect.Value, raw any, path string) error {
	switch v.Kind() {
	case reflect.Bool:
		switch x := raw.(type) {
		case bool:
			v.SetBool(x)
		case string:
			b, err := strconv.ParseBool(x)
			if err != nil {
				return fmt.Errorf("key %q: parse bool: %w", path, err)
			}
			v.SetBool(b)
		default:
			return fmt.Errorf("key %q: expected bool, got %T", path, raw)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := toInt(raw, path)
		if err != nil {
			return err
		}
		if v.OverflowInt(n) {
			return fmt.Errorf("key %q: %d overflows %s", path, n, v.Type())
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := toInt(raw, path)
		if err != nil {
			return err
		}
		if n < 0 || v.OverflowUint(uint64(n)) {
			return fmt.Errorf("key %q: %d overflows %s", path, n, v.Type())
		}
		v.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		f, err := toFloat(raw, path)
		if err != nil {
			return err
		}
		v.SetFloat(f)
	default:
		return fmt.Errorf("key %q: unsupported type %s", path, v.Type())
	}
	return nil
}

// toInt converts a raw YAML value to int64 (yaml.v3 yields int for
// integers, float64 for floats, string, bool).
func toInt(raw any, path string) (int64, error) {
	switch x := raw.(type) {
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("key %q: parse int: %w", path, err)
		}
		return n, nil
	}
	switch rv := reflect.ValueOf(raw); rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(rv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return int64(rv.Float()), nil
	}
	return 0, fmt.Errorf("key %q: expected number, got %T", path, raw)
}

// toFloat converts a raw YAML value to float64.
func toFloat(raw any, path string) (float64, error) {
	if s, ok := raw.(string); ok {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("key %q: parse float: %w", path, err)
		}
		return f, nil
	}
	switch rv := reflect.ValueOf(raw); rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return rv.Float(), nil
	}
	return 0, fmt.Errorf("key %q: expected number, got %T", path, raw)
}

// setDuration sets a time.Duration from "5s" or a number (milliseconds).
func setDuration(v reflect.Value, raw any, path string) error {
	if s, ok := raw.(string); ok {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("key %q: parse duration: %w", path, err)
		}
		v.SetInt(int64(d))
		return nil
	}
	n, err := toInt(raw, path)
	if err != nil {
		return err
	}
	v.SetInt(n * int64(time.Millisecond))
	return nil
}

// setString sets v from a string (env value or default tag).
func setString(v reflect.Value, s string, path string) error {
	if v.Type() == durationType {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("key %q: parse duration %q: %w", path, s, err)
		}
		v.SetInt(int64(d))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
		return nil
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("key %q: parse bool %q: %w", path, s, err)
		}
		v.SetBool(b)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return fmt.Errorf("key %q: parse int %q: %w", path, s, err)
		}
		if v.OverflowInt(n) {
			return fmt.Errorf("key %q: %d overflows %s", path, n, v.Type())
		}
		v.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return fmt.Errorf("key %q: parse uint %q: %w", path, s, err)
		}
		if v.OverflowUint(u) {
			return fmt.Errorf("key %q: %d overflows %s", path, u, v.Type())
		}
		v.SetUint(u)
		return nil
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("key %q: parse float %q: %w", path, s, err)
		}
		v.SetFloat(f)
		return nil
	case reflect.Slice:
		parts := strings.Split(s, ",")
		out := reflect.MakeSlice(v.Type(), len(parts), len(parts))
		for i, p := range parts {
			if err := setString(out.Index(i), strings.TrimSpace(p), path); err != nil {
				return err
			}
		}
		v.Set(out)
		return nil
	}
	return fmt.Errorf("key %q: cannot set %s from string", path, v.Type())
}

func toString(raw any) string {
	if s, ok := raw.(string); ok {
		return s
	}
	switch rv := reflect.ValueOf(raw); rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'f', -1, 64)
	case reflect.Bool:
		return strconv.FormatBool(rv.Bool())
	}
	return fmt.Sprint(raw)
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
