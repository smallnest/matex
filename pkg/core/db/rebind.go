package db

import "strings"

// rebind rewrites "?" placeholders to "$1".."$n" for PostgreSQL. MySQL and
// SQLite use "?" natively, so the query is returned unchanged. Quoted
// strings and comments are skipped, and "??" escapes a literal "?" (for
// Postgres's JSON `?` operator, e.g. `data ?? 'key'`).
//
// Keeping one placeholder style ("?") across drivers means business SQL is
// portable; only the driver-specific bits (DDL types, upsert syntax) need
// care.
func rebind(driver Driver, query string) string {
	if driver != Postgres {
		return query
	}
	return dollar(query)
}

func dollar(query string) string {
	// Fast path: nothing to rewrite.
	if !strings.ContainsRune(query, '?') {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	for i := 0; i < len(query); i++ {
		switch c := query[i]; c {
		case '\'', '"', '`':
			// Copy the quoted run verbatim (handles doubled-quote escapes).
			b.WriteByte(c)
			i++
			for i < len(query) {
				if query[i] == c {
					if i+1 < len(query) && query[i+1] == c { // doubled escape
						b.WriteByte(query[i])
						b.WriteByte(query[i+1])
						i += 2
						continue
					}
					b.WriteByte(query[i])
					break
				}
				b.WriteByte(query[i])
				i++
			}
		case '-':
			if i+1 < len(query) && query[i+1] == '-' { // line comment
				for i < len(query) && query[i] != '\n' {
					b.WriteByte(query[i])
					i++
				}
				if i < len(query) {
					b.WriteByte(query[i]) // newline
				}
			} else {
				b.WriteByte(c)
			}
		case '/':
			if i+1 < len(query) && query[i+1] == '*' { // block comment
				b.WriteByte(c)
				b.WriteByte(query[i+1])
				i += 2
				for i < len(query) {
					if query[i] == '*' && i+1 < len(query) && query[i+1] == '/' {
						b.WriteByte(query[i])
						b.WriteByte(query[i+1])
						i += 2
						break
					}
					b.WriteByte(query[i])
					i++
				}
				i--
			} else {
				b.WriteByte(c)
			}
		case '?':
			if i+1 < len(query) && query[i+1] == '?' { // "??" -> literal "?"
				b.WriteByte('?')
				i++
				break
			}
			n++
			b.WriteByte('$')
			b.WriteString(itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// itoa formats a small positive int without importing strconv's overhead in
// the hot path.
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
