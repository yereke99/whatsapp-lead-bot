package sqlite

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	sqlitedriver "modernc.org/sqlite"
)

// timeLayout is how every timestamp is stored.
//
// It is fixed-width, always UTC and always carries nine fractional digits, so
// SQLite's plain text comparison orders timestamps chronologically. A variable
// width layout would sort "…:05.9" after "…:05.12", which would quietly break
// every "scheduled_at <= now()" query in the scheduler.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// Layouts accepted when reading. The first is what this package writes; the
// rest cover values written by hand or by the sqlite3 CLI.
var timeLayouts = []string{
	timeLayout,
	"2006-01-02T15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

func init() {
	// These four keep the SQL readable and let the database do the work that
	// SQLite's own date functions cannot: they emit the storage layout above,
	// so a computed timestamp still compares correctly against a stored one.
	sqlitedriver.MustRegisterScalarFunction("now", 0,
		func(*sqlitedriver.FunctionContext, []driver.Value) (driver.Value, error) {
			return formatTime(time.Now()), nil
		})
	sqlitedriver.MustRegisterScalarFunction("gen_random_uuid", 0,
		func(*sqlitedriver.FunctionContext, []driver.Value) (driver.Value, error) {
			id, err := uuid.NewRandom()
			if err != nil {
				return nil, err
			}
			return id.String(), nil
		})
	// ts_add(timestamp, seconds) shifts a stored timestamp. The scheduler uses
	// it to recompute send times from a per-row step offset.
	sqlitedriver.MustRegisterScalarFunction("ts_add", 2, sqlTimestampAdd)
	// to_local_date(timestamp, iana_zone) is the analytics grouping key. Go's
	// zone database gives real local dates, including historical DST rules,
	// which SQLite alone cannot do.
	sqlitedriver.MustRegisterScalarFunction("to_local_date", 2, sqlToLocalDate)
}

func sqlTimestampAdd(_ *sqlitedriver.FunctionContext, args []driver.Value) (driver.Value, error) {
	if args[0] == nil || args[1] == nil {
		return nil, nil
	}
	text, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("ts_add: expected a timestamp, got %T", args[0])
	}
	base, err := parseTime(text)
	if err != nil {
		return nil, fmt.Errorf("ts_add: %w", err)
	}

	var seconds int64
	switch v := args[1].(type) {
	case int64:
		seconds = v
	case float64:
		seconds = int64(v)
	default:
		return nil, fmt.Errorf("ts_add: expected seconds, got %T", args[1])
	}
	return formatTime(base.Add(time.Duration(seconds) * time.Second)), nil
}

func sqlToLocalDate(_ *sqlitedriver.FunctionContext, args []driver.Value) (driver.Value, error) {
	if args[0] == nil {
		return nil, nil
	}
	text, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("to_local_date: expected a timestamp, got %T", args[0])
	}
	stamp, err := parseTime(text)
	if err != nil {
		return nil, fmt.Errorf("to_local_date: %w", err)
	}

	location := time.UTC
	if name, ok := args[1].(string); ok && name != "" {
		// An unknown zone falls back to UTC rather than failing the report.
		if loaded, err := time.LoadLocation(name); err == nil {
			location = loaded
		}
	}
	return stamp.In(location).Format(time.DateOnly), nil
}

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	// A bare number is a unix timestamp, which is what SQLite's own date
	// functions produce when asked for one.
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("parse timestamp %q", s)
}

// rewritePlaceholders converts Postgres $1 placeholders into SQLite's ?1 form.
//
// Both number their parameters from one and both allow a parameter to appear
// more than once, so the rewrite is exact rather than an approximation. Text
// inside string literals, quoted identifiers and comments is copied untouched.
func rewritePlaceholders(query string) string {
	if !strings.ContainsRune(query, '$') {
		return query
	}

	var b strings.Builder
	b.Grow(len(query))

	for i := 0; i < len(query); {
		switch c := query[i]; c {
		case '\'', '"', '`':
			i = copyQuoted(&b, query, i, c)
		case '-':
			if i+1 < len(query) && query[i+1] == '-' {
				for i < len(query) && query[i] != '\n' {
					b.WriteByte(query[i])
					i++
				}
				continue
			}
			b.WriteByte(c)
			i++
		case '/':
			if i+1 < len(query) && query[i+1] == '*' {
				end := strings.Index(query[i+2:], "*/")
				if end < 0 {
					b.WriteString(query[i:])
					return b.String()
				}
				stop := i + 2 + end + 2
				b.WriteString(query[i:stop])
				i = stop
				continue
			}
			b.WriteByte(c)
			i++
		case '$':
			j := i + 1
			for j < len(query) && query[j] >= '0' && query[j] <= '9' {
				j++
			}
			if j == i+1 { // a lone $, not a placeholder
				b.WriteByte(c)
				i++
				continue
			}
			b.WriteByte('?')
			b.WriteString(query[i+1 : j])
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// copyQuoted copies a quoted run verbatim, honouring doubled quotes as escapes,
// and returns the index just past it.
func copyQuoted(b *strings.Builder, query string, i int, quote byte) int {
	b.WriteByte(quote)
	i++
	for i < len(query) {
		if query[i] == quote {
			if i+1 < len(query) && query[i+1] == quote {
				b.WriteByte(quote)
				b.WriteByte(quote)
				i += 2
				continue
			}
			b.WriteByte(quote)
			return i + 1
		}
		b.WriteByte(query[i])
		i++
	}
	return i
}

// bindArgs converts query arguments into values the driver stores directly.
//
// Types database/sql already handles are passed through: uuid.UUID implements
// driver.Valuer, and named string types convert by reflection.
func bindArgs(args []any) ([]any, error) {
	if len(args) == 0 {
		return nil, nil
	}

	out := make([]any, len(args))
	for i, arg := range args {
		switch v := arg.(type) {
		case time.Time:
			out[i] = formatTime(v)
		case *time.Time:
			if v == nil {
				out[i] = nil
			} else {
				out[i] = formatTime(*v)
			}
		case []string:
			encoded, err := encodeStringSlice(v)
			if err != nil {
				return nil, err
			}
			out[i] = encoded
		case []uuid.UUID:
			// SQLite has no array parameters. Lists are passed as a JSON array
			// and expanded with json_each(), which keeps the query
			// parameterised instead of interpolating ids into the SQL.
			ids := make([]string, len(v))
			for n, id := range v {
				ids[n] = id.String()
			}
			encoded, err := encodeStringSlice(ids)
			if err != nil {
				return nil, err
			}
			out[i] = encoded
		case json.RawMessage:
			// json columns are TEXT guarded by json_valid(); binding the raw
			// bytes would store a BLOB that json_valid() rejects.
			if v == nil {
				out[i] = nil
			} else {
				out[i] = string(v)
			}
		case map[string]any:
			encoded, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("encode json argument: %w", err)
			}
			out[i] = string(encoded)
		default:
			out[i] = arg
		}
	}
	return out, nil
}

// scanRow scans one row, bridging the types SQLite cannot represent natively.
//
// Everything database/sql already handles goes straight through, including
// **uuid.UUID and **string for nullable columns.
func scanRow(rows *sql.Rows, dest []any) error {
	holders := make([]any, len(dest))
	var fixups []func() error

	for i, target := range dest {
		switch typed := target.(type) {
		case *time.Time:
			holder := &sql.NullString{}
			holders[i] = holder
			fixups = append(fixups, func() error {
				if !holder.Valid {
					*typed = time.Time{}
					return nil
				}
				parsed, err := parseTime(holder.String)
				if err != nil {
					return err
				}
				*typed = parsed
				return nil
			})

		case **time.Time:
			holder := &sql.NullString{}
			holders[i] = holder
			fixups = append(fixups, func() error {
				if !holder.Valid {
					*typed = nil
					return nil
				}
				parsed, err := parseTime(holder.String)
				if err != nil {
					return err
				}
				*typed = &parsed
				return nil
			})

		case *[]string:
			holder := &sql.NullString{}
			holders[i] = holder
			fixups = append(fixups, func() error {
				values, err := decodeStringSlice(holder)
				if err != nil {
					return err
				}
				*typed = values
				return nil
			})

		case *json.RawMessage:
			holder := &sql.NullString{}
			holders[i] = holder
			fixups = append(fixups, func() error {
				if !holder.Valid {
					*typed = nil
					return nil
				}
				*typed = json.RawMessage(holder.String)
				return nil
			})

		default:
			holders[i] = target
		}
	}

	if err := rows.Scan(holders...); err != nil {
		return err
	}
	for _, fixup := range fixups {
		if err := fixup(); err != nil {
			return err
		}
	}
	return nil
}

// encodeStringSlice stores a list as a JSON array, which keeps it queryable
// through SQLite's json functions and round-trips values containing commas.
func encodeStringSlice(values []string) (string, error) {
	if values == nil {
		return "[]", nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode string list: %w", err)
	}
	return string(encoded), nil
}

func decodeStringSlice(holder *sql.NullString) ([]string, error) {
	if !holder.Valid || strings.TrimSpace(holder.String) == "" {
		return nil, nil
	}
	var values []string
	if err := json.Unmarshal([]byte(holder.String), &values); err != nil {
		return nil, fmt.Errorf("decode string list %q: %w", holder.String, err)
	}
	return values, nil
}
