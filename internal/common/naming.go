package common

import "strings"

// Export upper-cases the first rune ("nav" → "Nav"). One home for
// plan.interfaceName and gen.exportName (A3.2).
func Export(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// LowerFirst lower-cases the first rune ("NavController" → "navController").
// The receiver-name convention (gen.lowerFirst).
func LowerFirst(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return strings.ToLower(string(r[0])) + string(r[1:])
}

// CamelGo renders a delimited identifier as exported CamelCase, lowercasing
// each segment first ("DEMO_ACC", "demo.acc", "demo acc" → "DemoAcc").
// Splits on '_' '.' ' '. The Go-path policy (plan.camel) — byte-identical
// moves only; the other casing policies are deliberately distinct (AD3).
func CamelGo(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '.' || r == ' ' })
	var sb strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		r := []rune(strings.ToLower(p))
		sb.WriteRune([]rune(strings.ToUpper(string(r[0])))[0])
		sb.WriteString(string(r[1:]))
	}
	return sb.String()
}

// CamelLowerGo renders a snake_case identifier with the first segment kept
// verbatim and later segments capitalized ("comp_cd" → "compCd"). The Go
// field-name policy (gen.camelLower) — the inverse of Export∘CamelGo.
func CamelLowerGo(s string) string {
	parts := strings.Split(s, "_")
	var sb strings.Builder
	for i, p := range parts {
		if p == "" {
			continue
		}
		if i == 0 {
			sb.WriteString(p)
			continue
		}
		r := []rune(p)
		sb.WriteRune([]rune(strings.ToUpper(string(r[0])))[0])
		sb.WriteString(string(r[1:]))
	}
	return sb.String()
}

// CamelPy renders an identifier as CamelCase preserving inner case, after
// sanitizing non-identifier bytes to '_' ("FML_MF-NAV" → "FMLMfNav"-shaped).
// The Python-path policy (pyplan.Camel) — case-preserving where CamelGo
// lowercases; splits on '_' '-' '.' (AD3: the policies are load-bearing
// and distinct).
func CamelPy(s string) string {
	parts := strings.FieldsFunc(PyIdent(s), func(r rune) bool { return r == '_' || r == '-' || r == '.' })
	var sb strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		sb.WriteString(strings.ToUpper(p[:1]))
		sb.WriteString(p[1:])
	}
	return sb.String()
}

// PyIdent maps every non [_a-zA-Z0-9] byte to '_' — the Python-identifier
// sanitizer pyplan.Camel composes with (pyplan.pyIdent).
func PyIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
