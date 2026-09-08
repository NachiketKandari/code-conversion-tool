// FixtureSource seam (PRD-2026-09-09 GT-D5): the values tests assert on come
// from a pluggable backend. v0 is AssumedFixtureSource — deterministic
// placeholder values synthesized from the extracted shapes — so -no-llm
// runs are byte-identical everywhere. The real backend (GT-7, on user logs)
// swaps realism in without touching the pipeline.
package testgen

import (
	"fmt"
	"strings"
)

// FixtureSource provides assumed fixture values for one service.
type FixtureSource interface {
	// RowValues returns one mock row for the db-tag columns, in order.
	RowValues(cols []string) []string
	// ColExpr renders the expected-struct field literal for a db column of
	// the given Go type, e.g. sql.NullString{String: "comp_cd", Valid: true}.
	ColExpr(col, typ string) string
	// ArgValue renders a store-call bind literal for a param of the given
	// type, e.g. "comp_cd" / time.Now() / 0.
	ArgValue(name, typ string) string
	// FieldValues returns the json-tagged fields of a models struct as
	// (field name, assumed value) pairs, in declaration order.
	FieldValues(structName string) [][2]string
	// ZeroExpr renders the zero value literal for a scalar Go type.
	ZeroExpr(scalar string) string
}

// AssumedFixtureSource synthesizes deterministic placeholders: db values are
// the lowercased column name, bind literals the lowercased param name, json
// values the lowercased field name. Every consumer of a value derives both
// sides (mock row and expected expr) from the same source, so asserted
// equality always matches.
type AssumedFixtureSource struct {
	Models *modelsInfo
}

func placeholder(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if r >= 'A' && r <= 'Z' {
			b.WriteRune(r + 32)
		}
	}
	if b.Len() == 0 {
		return "val"
	}
	return b.String()
}

// RowValues implements FixtureSource.
func (a *AssumedFixtureSource) RowValues(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = placeholder(c)
	}
	return out
}

// ColExpr implements FixtureSource.
func (a *AssumedFixtureSource) ColExpr(col, typ string) string {
	v := placeholder(col)
	switch typ {
	case "sql.NullString":
		return fmt.Sprintf("sql.NullString{String: %q, Valid: true}", v)
	case "sql.NullInt64":
		return fmt.Sprintf("sql.NullInt64{Int64: 0, Valid: true}")
	case "sql.NullTime":
		return "sql.NullTime{Time: time.Date(2025, 4, 28, 0, 0, 0, 0, time.UTC), Valid: true}"
	default:
		return fmt.Sprintf("%q", v)
	}
}

// ArgValue implements FixtureSource.
func (a *AssumedFixtureSource) ArgValue(name, typ string) string {
	switch typ {
	case "string":
		return fmt.Sprintf("%q", placeholder(name))
	case "time.Time":
		return "time.Now()"
	case "int", "int64", "float64":
		return "0"
	default:
		return "nil"
	}
}

// FieldValues implements FixtureSource. Tag options (,omitempty) are
// stripped before placeholdering — values derive from the field name only.
func (a *AssumedFixtureSource) FieldValues(structName string) [][2]string {
	fields := a.Models.Structs[structName]
	out := make([][2]string, 0, len(fields))
	for _, f := range fields {
		if f.JSON == "" {
			continue
		}
		name := f.JSON
		if i := strings.Index(name, ","); i >= 0 {
			name = name[:i]
		}
		out = append(out, [2]string{f.Name, placeholder(name)})
	}
	return out
}

// ZeroExpr implements FixtureSource.
func (a *AssumedFixtureSource) ZeroExpr(scalar string) string {
	switch scalar {
	case "string":
		return `""`
	case "float64":
		return "0"
	default:
		return "0"
	}
}
