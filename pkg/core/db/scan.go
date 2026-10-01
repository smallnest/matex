package db

import (
	"database/sql"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Struct scanning: map result columns to struct fields by `db` tag, falling
// back to the field name (case-insensitive). Unknown columns are ignored,
// which keeps `SELECT *` working as a table gains columns.

var (
	fieldCache  sync.Map // reflect.Type -> map[string]int (lower(column) -> field index)
	timeType    = reflect.TypeOf(time.Time{})
	scannerType = reflect.TypeOf((*sql.Scanner)(nil)).Elem()
)

func fieldIndex(t reflect.Type) map[string]int {
	if m, ok := fieldCache.Load(t); ok {
		return m.(map[string]int)
	}
	m := make(map[string]int, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Tag.Get("db")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		m[strings.ToLower(name)] = i
	}
	fieldCache.Store(t, m)
	return m
}

// scanAll scans every remaining row into []T.
func scanAll[T any](rows *sql.Rows) ([]T, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, wrapErr(err)
	}
	var out []T
	for rows.Next() {
		v, err := scanRow[T](rows, cols)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapErr(err)
	}
	return out, nil
}

// scanRow scans the current row into a T via db tags.
func scanRow[T any](rows *sql.Rows, cols []string) (T, error) {
	var zero T
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return zero, wrapErr(err)
	}
	var v T
	if err := assignRow(cols, vals, &v); err != nil {
		return zero, err
	}
	return v, nil
}

func assignRow(cols []string, vals []any, dst any) error {
	rv := reflect.ValueOf(dst).Elem()
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("db: scan target must be a struct, got %s", rv.Kind())
	}
	idx := fieldIndex(rv.Type())
	for i, col := range cols {
		fi, ok := idx[strings.ToLower(col)]
		if !ok {
			continue
		}
		if err := assign(rv.Field(fi), vals[i]); err != nil {
			return fmt.Errorf("db: column %q: %w", col, err)
		}
	}
	return nil
}

// assign copies a driver value (as produced by scanning into *any) into fv.
func assign(fv reflect.Value, val any) error {
	if !fv.CanSet() {
		return fmt.Errorf("field %s is not settable", fv.Type())
	}
	if fv.Kind() == reflect.Pointer {
		if val == nil {
			fv.SetZero()
			return nil
		}
		if fv.IsNil() {
			fv.Set(reflect.New(fv.Type().Elem()))
		}
		return assign(fv.Elem(), val)
	}
	if val == nil {
		fv.SetZero()
		return nil
	}
	if fv.Type() == timeType {
		return assignTime(fv, val)
	}
	// sql.Null* (and any custom sql.Scanner): let the type decode itself.
	if fv.Kind() == reflect.Struct && fv.CanAddr() && fv.Addr().Type().Implements(scannerType) {
		return fv.Addr().Interface().(sql.Scanner).Scan(val)
	}
	switch fv.Kind() {
	case reflect.String:
		s, err := toString(val)
		if err != nil {
			return err
		}
		fv.SetString(s)
	case reflect.Bool:
		b, err := toBool(val)
		if err != nil {
			return err
		}
		fv.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := toInt64(val)
		if err != nil {
			return err
		}
		if fv.OverflowInt(n) {
			return fmt.Errorf("value %d overflows %s", n, fv.Type())
		}
		fv.SetInt(n)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := toInt64(val)
		if err != nil {
			return err
		}
		if n < 0 {
			return fmt.Errorf("negative value %d for %s", n, fv.Type())
		}
		if fv.OverflowUint(uint64(n)) {
			return fmt.Errorf("value %d overflows %s", n, fv.Type())
		}
		fv.SetUint(uint64(n))
	case reflect.Float32, reflect.Float64:
		f, err := toFloat64(val)
		if err != nil {
			return err
		}
		fv.SetFloat(f)
	case reflect.Slice:
		if fv.Type().Elem().Kind() != reflect.Uint8 {
			return fmt.Errorf("unsupported slice type %s", fv.Type())
		}
		b, err := toBytes(val)
		if err != nil {
			return err
		}
		fv.SetBytes(b)
	default:
		return fmt.Errorf("unsupported field type %s", fv.Type())
	}
	return nil
}

func assignTime(fv reflect.Value, val any) error {
	switch t := val.(type) {
	case time.Time:
		fv.Set(reflect.ValueOf(t))
	case string:
		for _, layout := range []string{
			time.RFC3339Nano,
			"2006-01-02 15:04:05.999999999-07:00",
			"2006-01-02 15:04:05.999999999",
			"2006-01-02 15:04:05",
			"2006-01-02",
		} {
			if ts, err := time.Parse(layout, t); err == nil {
				fv.Set(reflect.ValueOf(ts))
				return nil
			}
		}
		return fmt.Errorf("cannot parse %q as time.Time", t)
	case []byte:
		return assignTime(fv, string(t))
	default:
		return fmt.Errorf("cannot convert %T to time.Time", val)
	}
	return nil
}

func toString(val any) (string, error) {
	switch v := val.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case bool:
		return strconv.FormatBool(v), nil
	case time.Time:
		return v.Format(time.RFC3339Nano), nil
	default:
		return "", fmt.Errorf("cannot convert %T to string", val)
	}
}

func toInt64(val any) (int64, error) {
	switch v := val.(type) {
	case int64:
		return v, nil
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case float64:
		return int64(v), nil
	case []byte:
		return strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
	case string:
		return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", val)
	}
}

func toFloat64(val any) (float64, error) {
	switch v := val.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case []byte:
		return strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
	case string:
		return strconv.ParseFloat(strings.TrimSpace(v), 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", val)
	}
}

func toBool(val any) (bool, error) {
	switch v := val.(type) {
	case bool:
		return v, nil
	case int64:
		return v != 0, nil
	case []byte:
		return strconv.ParseBool(strings.TrimSpace(string(v)))
	case string:
		return strconv.ParseBool(strings.TrimSpace(v))
	default:
		return false, fmt.Errorf("cannot convert %T to bool", val)
	}
}

func toBytes(val any) ([]byte, error) {
	switch v := val.(type) {
	case []byte:
		return v, nil
	case string:
		return []byte(v), nil
	default:
		return nil, fmt.Errorf("cannot convert %T to []byte", val)
	}
}
