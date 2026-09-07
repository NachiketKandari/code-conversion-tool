// Package pred parses C boolean/comparison condition text (branch
// conditions, PF-2) into a small expression tree: ||, &&, !, comparisons,
// calls, literals, identifiers. Grammar = C boolean/comparison expressions
// only; precedence follows C (|| lowest, then &&, then !/comparison unary
// level, right-associative !). Anything outside that grammar degrades to
// Raw{text} — a parse is never a hard failure, so segmentation continues on
// the raw text.
//
// The tree is a single recursive struct (Expr) rather than a type hierarchy
// so it serializes to the IR JSON without custom code; Kind is the node
// discriminator and the vocabulary maps as: Or{Items}, And{Items},
// Not{Inner}, Cmp{L, Op, R}, Call{Name, Args}, Lit{Text}, Ident{Name},
// Raw{Text}.
package pred

import (
	"strconv"
	"strings"
	"unicode"
)

// Kind is the Expr node discriminator.
type Kind string

const (
	KindOr    Kind = "or"
	KindAnd   Kind = "and"
	KindNot   Kind = "not"
	KindCmp   Kind = "cmp"
	KindCall  Kind = "call"
	KindLit   Kind = "lit"
	KindIdent Kind = "ident"
	KindRaw   Kind = "raw"
)

// Expr is one node of the parsed condition tree. Which fields are set
// depends on Kind: or/and → Items, not → Inner, cmp → L/Op/R, call →
// Name/Args, ident → Name, lit/raw → Text.
type Expr struct {
	Kind  Kind     `json:"kind"`
	Items []Expr   `json:"items,omitempty"` // or, and
	Inner *Expr    `json:"inner,omitempty"` // not
	L     *Expr    `json:"l,omitempty"`     // cmp left
	R     *Expr    `json:"r,omitempty"`     // cmp right
	Op    string   `json:"op,omitempty"`    // cmp operator (==, !=, >, <, >=, <=)
	Name  string   `json:"name,omitempty"`  // call callee / ident name
	Args  []string `json:"args,omitempty"`  // call args, raw text per top-level comma
	Text  string   `json:"text,omitempty"`  // lit text / raw degraded text
}

// Parse parses C boolean/comparison condition text into a tree. Never
// fails: text outside the grammar comes back as Raw{text} (whitespace-
// normalized).
func Parse(text string) Expr {
	t := newTokenizer(text)
	e, ok := t.parseOr()
	if !ok || !t.atEnd() {
		return Raw(text)
	}
	return e
}

// Raw builds the degrade node for text the grammar does not cover.
func Raw(text string) Expr {
	return Expr{Kind: KindRaw, Text: strings.Join(strings.Fields(text), " ")}
}

// Ident builds an identifier leaf.
func Ident(name string) Expr { return Expr{Kind: KindIdent, Name: name} }

// IsRaw reports whether the expression is the Raw degrade.
func (e *Expr) IsRaw() bool { return e != nil && e.Kind == KindRaw }

// Idents returns the expression's Ident leaves in appearance order,
// deduplicated. Raw nodes contribute nothing — their content is untrusted
// text (call args included, a Call's args stay raw strings).
func Idents(e *Expr) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(x *Expr)
	walk = func(x *Expr) {
		if x == nil {
			return
		}
		switch x.Kind {
		case KindOr, KindAnd:
			for i := range x.Items {
				walk(&x.Items[i])
			}
		case KindNot:
			walk(x.Inner)
		case KindCmp:
			walk(x.L)
			walk(x.R)
		case KindIdent:
			if !seen[x.Name] {
				seen[x.Name] = true
				out = append(out, x.Name)
			}
		}
	}
	walk(e)
	return out
}

// --- tokenizer + recursive-descent parser ---

type tokenizer struct {
	src string
	pos int
}

func newTokenizer(src string) tokenizer { return tokenizer{src: src} }

func (t *tokenizer) atEnd() bool { return t.pos >= len(t.src) }

func (t *tokenizer) ws() {
	for t.pos < len(t.src) && unicode.IsSpace(rune(t.src[t.pos])) {
		t.pos++
	}
}

// op consumes one of the given multi-char operators (longest first).
func (t *tokenizer) op(ops ...string) (string, bool) {
	t.ws()
	for _, o := range ops {
		if strings.HasPrefix(t.src[t.pos:], o) {
			t.pos += len(o)
			return o, true
		}
	}
	return "", false
}

func (t *tokenizer) peekByte() byte {
	if t.pos < len(t.src) {
		return t.src[t.pos]
	}
	return 0
}

// parseOr := parseAnd ('||' parseAnd)*   (|| binds loosest)
func (t *tokenizer) parseOr() (Expr, bool) {
	first, ok := t.parseAnd()
	if !ok {
		return Expr{}, false
	}
	items := []Expr{first}
	for {
		if _, ok := t.op("||"); !ok {
			break
		}
		next, ok := t.parseAnd()
		if !ok {
			return Expr{}, false
		}
		items = append(items, next)
	}
	if len(items) == 1 {
		return items[0], true
	}
	return Expr{Kind: KindOr, Items: items}, true
}

// parseAnd := parseCmp ('&&' parseCmp)*
func (t *tokenizer) parseAnd() (Expr, bool) {
	first, ok := t.parseCmp()
	if !ok {
		return Expr{}, false
	}
	items := []Expr{first}
	for {
		if _, ok := t.op("&&"); !ok {
			break
		}
		next, ok := t.parseCmp()
		if !ok {
			return Expr{}, false
		}
		items = append(items, next)
	}
	if len(items) == 1 {
		return items[0], true
	}
	return Expr{Kind: KindAnd, Items: items}, true
}

var cmpOps = []string{"==", "!=", ">=", "<=", ">", "<"}

// parseCmp := parseUnary [cmpOp parseUnary] — at most one comparison; a
// second (a < b < c) is outside the grammar → fail → Raw.
func (t *tokenizer) parseCmp() (Expr, bool) {
	left, ok := t.parseUnary()
	if !ok {
		return Expr{}, false
	}
	op, ok := t.op(cmpOps...)
	if !ok {
		return left, true
	}
	right, ok := t.parseUnary()
	if !ok {
		return Expr{}, false
	}
	// Arithmetic or ternary continuation leaves the grammar; a doubled
	// | or & is the legitimate && / || continuation, a lone one is not.
	if !t.atEnd() {
		t.ws()
		switch b := t.peekByte(); b {
		case '|', '&':
			if t.pos+1 >= len(t.src) || t.src[t.pos+1] != b {
				return Expr{}, false
			}
		case '+', '-', '*', '/', '%', '?', ':', '=':
			return Expr{}, false
		}
	}
	return Expr{Kind: KindCmp, L: &left, Op: op, R: &right}, true
}

// parseUnary := '!' parseUnary | parsePrimary   (C's right-associative !)
func (t *tokenizer) parseUnary() (Expr, bool) {
	if _, ok := t.op("!"); ok {
		inner, ok := t.parseUnary()
		if !ok {
			return Expr{}, false
		}
		return Expr{Kind: KindNot, Inner: &inner}, true
	}
	return t.parsePrimary()
}

// parsePrimary := '(' parseOr ')' | call | literal | ident
func (t *tokenizer) parsePrimary() (Expr, bool) {
	t.ws()
	if t.atEnd() {
		return Expr{}, false
	}
	if t.peekByte() == '(' {
		t.pos++
		inner, ok := t.parseOr()
		if !ok {
			return Expr{}, false
		}
		if _, ok := t.op(")"); !ok {
			return Expr{}, false
		}
		return inner, true
	}
	switch b := t.peekByte(); {
	case b == '\'' || b == '"':
		lit, ok := t.literal()
		return lit, ok
	case b == '-' || b == '+':
		// A signed literal comparand (`== -1`). Arithmetic continuing an
		// operand is rejected at the comparison level instead.
		return t.number()
	case b >= '0' && b <= '9':
		return t.number()
	case b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z'):
		return t.identOrCall()
	}
	return Expr{}, false
}

// literal consumes a quoted C literal (escapes honored) as a Lit node.
func (t *tokenizer) literal() (Expr, bool) {
	quote := t.src[t.pos]
	start := t.pos
	t.pos++
	for t.pos < len(t.src) {
		if t.src[t.pos] == '\\' {
			t.pos += 2
			continue
		}
		if t.src[t.pos] == quote {
			t.pos++
			return Expr{Kind: KindLit, Text: t.src[start:t.pos]}, true
		}
		t.pos++
	}
	return Expr{}, false
}

// number consumes a decimal integer/float, including a preceding sign glued
// to it (the `-1` comparand idiom).
func (t *tokenizer) number() (Expr, bool) {
	t.ws()
	start := t.pos
	if t.peekByte() == '-' || t.peekByte() == '+' {
		t.pos++
	}
	digits := 0
	for t.pos < len(t.src) {
		b := t.src[t.pos]
		if b >= '0' && b <= '9' {
			digits++
			t.pos++
			continue
		}
		if b == '.' && digits > 0 {
			t.pos++
			continue
		}
		break
	}
	if digits == 0 {
		return Expr{}, false
	}
	if t.pos < len(t.src) && isIdentChar(t.src[t.pos]) {
		// e.g. 100L — suffix beyond the grammar.
		return Expr{}, false
	}
	return Expr{Kind: KindLit, Text: t.src[start:t.pos]}, true
}

// identOrCall consumes an identifier, or a call with its raw-text argument
// list (call args are opaque strings — their internals are not boolean
// structure).
func (t *tokenizer) identOrCall() (Expr, bool) {
	start := t.pos
	for t.pos < len(t.src) && isIdentChar(t.src[t.pos]) {
		t.pos++
	}
	name := t.src[start:t.pos]
	t.ws()
	if t.peekByte() != '(' {
		return Expr{Kind: KindIdent, Name: name}, true
	}
	t.pos++ // (
	args, ok := t.balancedArgs()
	if !ok {
		return Expr{}, false
	}
	return Expr{Kind: KindCall, Name: name, Args: args}, true
}

// balancedArgs consumes a call's argument list assuming the opening '(' was
// consumed, returning the args as raw text split on top-level commas.
func (t *tokenizer) balancedArgs() ([]string, bool) {
	depth := 1
	var args []string
	var cur strings.Builder
	for t.pos < len(t.src) {
		b := t.src[t.pos]
		switch b {
		case '(':
			depth++
			cur.WriteByte(b)
		case ')':
			depth--
			if depth == 0 {
				t.pos++
				if strings.TrimSpace(cur.String()) != "" {
					args = append(args, cur.String())
				}
				return args, true
			}
			cur.WriteByte(b)
		case '"', '\'':
			quote := b
			cur.WriteByte(b)
			t.pos++
			for t.pos < len(t.src) {
				c := t.src[t.pos]
				cur.WriteByte(c)
				if c == '\\' && t.pos+1 < len(t.src) {
					t.pos++
					cur.WriteByte(t.src[t.pos])
				} else if c == quote {
					break
				}
				t.pos++
			}
		case ',':
			if depth == 1 {
				args = append(args, cur.String())
				cur.Reset()
				t.pos++
				continue
			}
			cur.WriteByte(b)
		default:
			cur.WriteByte(b)
		}
		t.pos++
	}
	return nil, false
}

func isIdentChar(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// String renders the tree back to normalized condition text — the audit
// trail's compact form (raw text stays in Condition.Expr).
func (e *Expr) String() string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case KindOr:
		parts := make([]string, len(e.Items))
		for i := range e.Items {
			parts[i] = e.Items[i].String()
		}
		return "(" + strings.Join(parts, " || ") + ")"
	case KindAnd:
		parts := make([]string, len(e.Items))
		for i := range e.Items {
			parts[i] = e.Items[i].String()
		}
		return "(" + strings.Join(parts, " && ") + ")"
	case KindNot:
		return "!" + e.Inner.String()
	case KindCmp:
		return e.L.String() + " " + e.Op + " " + e.R.String()
	case KindCall:
		return e.Name + "(" + strings.Join(e.Args, ", ") + ")"
	case KindIdent:
		return e.Name
	case KindLit:
		return e.Text
	case KindRaw:
		return e.Text
	}
	return strconv.Quote(e.Text)
}
