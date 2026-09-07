package pred

import (
	"reflect"
	"testing"
)

// TestParsePrecedence pins C operator precedence on adversarial chains
// (PF-2.4): || loosest, && tighter, ! right-associative and tighter than
// comparison, parens override.
func TestParsePrecedence(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Expr
	}{
		{
			// a == 'Y' || b == 'H' && !c  →  a=='Y' || (b=='H' && !c)
			name: "or binds loosest",
			in:   `a == 'Y' || b == 'H' && !c`,
			want: Expr{Kind: KindOr, Items: []Expr{
				{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "a"}, Op: "==", R: &Expr{Kind: KindLit, Text: "'Y'"}},
				{Kind: KindAnd, Items: []Expr{
					{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "b"}, Op: "==", R: &Expr{Kind: KindLit, Text: "'H'"}},
					{Kind: KindNot, Inner: &Expr{Kind: KindIdent, Name: "c"}},
				}},
			}},
		},
		{
			// Parenthesized override: (a == 'Y' || b == 'H') && !c
			name: "parens override",
			in:   `(a == 'Y' || b == 'H') && !c`,
			want: Expr{Kind: KindAnd, Items: []Expr{
				{Kind: KindOr, Items: []Expr{
					{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "a"}, Op: "==", R: &Expr{Kind: KindLit, Text: "'Y'"}},
					{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "b"}, Op: "==", R: &Expr{Kind: KindLit, Text: "'H'"}},
				}},
				{Kind: KindNot, Inner: &Expr{Kind: KindIdent, Name: "c"}},
			}},
		},
		{
			// C's ! binds tighter than ==: !a == b is (!a) == b.
			name: "not tighter than cmp",
			in:   `!a == b`,
			want: Expr{Kind: KindCmp,
				L:  &Expr{Kind: KindNot, Inner: &Expr{Kind: KindIdent, Name: "a"}},
				Op: "==", R: &Expr{Kind: KindIdent, Name: "b"}},
		},
		{
			// But a parenthesized comparison under ! keeps the tree.
			name: "not over parens",
			in:   `!(a == b)`,
			want: Expr{Kind: KindNot, Inner: &Expr{Kind: KindCmp,
				L: &Expr{Kind: KindIdent, Name: "a"}, Op: "==", R: &Expr{Kind: KindIdent, Name: "b"}}},
		},
		{
			// Double negation is right-associative.
			name: "double not",
			in:   `!!done`,
			want: Expr{Kind: KindNot, Inner: &Expr{Kind: KindNot, Inner: &Expr{Kind: KindIdent, Name: "done"}}},
		},
		{
			// Chain of || flattens into one Or node.
			name: "or chain",
			in:   `a == 'I' || a == 'F' || a == 'H'`,
			want: Expr{Kind: KindOr, Items: []Expr{
				{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "a"}, Op: "==", R: &Expr{Kind: KindLit, Text: "'I'"}},
				{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "a"}, Op: "==", R: &Expr{Kind: KindLit, Text: "'F'"}},
				{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "a"}, Op: "==", R: &Expr{Kind: KindLit, Text: "'H'"}},
			}},
		},
		{
			// Calls nest inside comparisons; call args stay raw strings.
			name: "nested calls",
			in:   `FNOTPRES(Ferror32) || Fget32(buf,FIELD,0,(char*)&v,0) == -1`,
			want: Expr{Kind: KindOr, Items: []Expr{
				{Kind: KindCall, Name: "FNOTPRES", Args: []string{"Ferror32"}},
				{Kind: KindCmp,
					L:  &Expr{Kind: KindCall, Name: "Fget32", Args: []string{"buf", "FIELD", "0", "(char*)&v", "0"}},
					Op: "==", R: &Expr{Kind: KindLit, Text: "-1"}},
			}},
		},
		{
			name: "count comparisons",
			in:   `cnt >= 1 && cnt <= 9`,
			want: Expr{Kind: KindAnd, Items: []Expr{
				{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "cnt"}, Op: ">=", R: &Expr{Kind: KindLit, Text: "1"}},
				{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "cnt"}, Op: "<=", R: &Expr{Kind: KindLit, Text: "9"}},
			}},
		},
		{
			name: "bare ident condition",
			in:   `done`,
			want: Expr{Kind: KindIdent, Name: "done"},
		},
		{
			name: "negative comparand",
			in:   `x == -1`,
			want: Expr{Kind: KindCmp, L: &Expr{Kind: KindIdent, Name: "x"}, Op: "==", R: &Expr{Kind: KindLit, Text: "-1"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Parse(%q) =\n got  %s\n want %s", tc.in, got.String(), tc.want.String())
			}
		})
	}
}

// TestParseRawDegrade: anything outside the C boolean/comparison grammar —
// arithmetic, assignments, chained comparisons — degrades to Raw, never a
// hard failure.
func TestParseRawDegrade(t *testing.T) {
	for _, in := range []string{
		"a + 1 == 2",           // arithmetic operand
		"ptr = tpalloc(x)",     // assignment
		"a < b < c",            // chained comparison
		"x ? y : z",            // ternary
		"(a == 1",              // unbalanced parens
		"a == 'Y' &&",          // dangling operator
		"(FBFR32*)buf == NULL", // casted comparand
		"a & 0x01",             // bitwise
	} {
		got := Parse(in)
		if !got.IsRaw() {
			t.Errorf("Parse(%q) degraded to %s, want raw", in, got.String())
		}
		if got.Text == "" {
			t.Errorf("raw node for %q lost the original text", in)
		}
	}
}

func TestIdents(t *testing.T) {
	tree := Parse(`(a == 'Y' || b == 'H') && !a && FNOTPRES(x)`)
	got := Idents(&tree)
	if want := []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("idents = %v, want %v (call args stay opaque)", got, want)
	}
	raw := Raw("x == 1")
	if got := Idents(&raw); got != nil {
		t.Errorf("raw idents = %v, want none", got)
	}
}

func TestExprStringRoundTrip(t *testing.T) {
	in := `a == 'Y' || b == 'H' && !c`
	tree := Parse(in)
	if got, want := tree.String(), "(a == 'Y' || (b == 'H' && !c))"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
