package scanner

import (
	"reflect"
	"testing"
)

// pf1Comments indexes the fixture's comments by start line.
func pf1Comments(facts *SourceFacts) map[int]*Comment {
	m := map[int]*Comment{}
	for i := range facts.Comments {
		m[facts.Comments[i].StartLine] = &facts.Comments[i]
	}
	return m
}

// TestCommentInventory pins the PF-1 facts on the adversarial fixture:
// comment spans are first-class (block/line/banner kinds with extents),
// comment content is opaque to brace/paren accounting, C block comments do
// not nest (first */), banner-delimited regions stay live, and
// comment-located code never pollutes recorded facts.
func TestCommentInventory(t *testing.T) {
	facts, err := ScanFile("../../../testdata/pf/comment_traps.pc")
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	c := pf1Comments(facts)

	// Header block comment spans lines 1-3.
	if header := c[1]; header == nil || header.Kind != CommentBlock || header.EndLine != 3 {
		t.Errorf("header comment = %+v, want block 1-3", c[1])
	}
	// Banner 'added here' marker: single line → Live (delimits live code).
	if added := c[15]; added == nil || added.Kind != CommentBanner || !added.Live {
		t.Errorf("banner added comment = %+v, want banner Live", c[15])
	}
	// Banner 'comment ends' marker: single line → Live.
	if ends := c[20]; ends == nil || ends.Kind != CommentBanner || !ends.Live {
		t.Errorf("banner ends comment = %+v, want banner Live", c[20])
	}
	// The multi-line "commented … comment ends" variant wraps dead content:
	// banner kind, Live false, spanning 24-26.
	if com := c[24]; com == nil || com.Kind != CommentBanner || com.Live || com.EndLine != 26 {
		t.Errorf("banner commented comment = %+v, want banner 24-26 not Live", c[24])
	}
	// Line comment ends on its own line.
	if line := c[12]; line == nil || line.Kind != CommentLine || line.EndLine != 12 {
		t.Errorf("line comment = %+v", c[12])
	}

	// InComment: dead SQL inside the banner-commented block is in a comment;
	// live code inside the banner-delimited region and beside a trailing
	// comment is not.
	if !facts.InComment(25, 13) {
		t.Error("line 25 (dead SQL inside banner-commented block) not reported in comment")
	}
	if facts.InComment(16, 9) {
		t.Error("line 16 (live code between banners) reported in comment")
	}
	if facts.InComment(13, 5) {
		t.Error("line 13 col 5 (code before trailing comment) reported in comment")
	}
	if facts.InComment(27, 9) {
		t.Error("line 27 (code after banner-commented block) reported in comment")
	}

	// Opaque accounting: braces/parens/quotes inside comments never confused
	// the scanner — the two live if-blocks resolved; the commented if never
	// became a branch.
	if len(facts.Branches) != 2 {
		t.Fatalf("branches = %d, want 2: %+v", len(facts.Branches), facts.Branches)
	}
	if facts.Branches[0].Cond != "dead_fn(x) == -1" {
		t.Errorf("live-banner if cond = %q", facts.Branches[0].Cond)
	}
	if facts.Branches[1].Cond != "x == 1" || facts.Branches[1].BlockStart != 23 || facts.Branches[1].BlockEnd != 28 {
		t.Errorf("trailing-comment if = %+v", facts.Branches[1])
	}

	// Comment-located SQL never pollutes recorded facts (PF-1.5) while the
	// live SELECT is recorded.
	if len(facts.Queries) != 1 || facts.Queries[0].Normalized != "SELECT 2 INTO :c_flag FROM DUAL" {
		t.Errorf("queries = %v, want only the live SELECT 2", queriesOf(facts))
	}

	// Live code between banners is recorded (the v0.6.2 lesson).
	var foundDead bool
	for _, call := range facts.Calls {
		if call.Name == "dead_fn" {
			foundDead = true
		}
	}
	if !foundDead {
		t.Error("dead_fn (live code between banners) missing from calls")
	}

	// The healthy fixture records zero unbalanced regions.
	if len(facts.Unbalanced) != 0 {
		t.Errorf("unbalanced = %v, want none", facts.Unbalanced)
	}
}

// TestUnbalancedFacts pins PF-1.4: unterminated block comment, unterminated
// EXEC SQL, and unbalanced braces are loud recorded facts — never a silent
// truncation (the v0.6.2 lesson, generalized).
func TestUnbalancedFacts(t *testing.T) {
	facts, err := ScanFile("../../../testdata/pf/unbalanced.pc")
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	var kinds []string
	for _, u := range facts.Unbalanced {
		kinds = append(kinds, u.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"exec_sql", "braces"}) {
		t.Errorf("unbalanced kinds = %v, want [exec_sql braces]", kinds)
	}
	// The partial EXEC SQL is never passed off as a statement.
	if len(facts.AllSQL) != 0 || len(facts.Queries) != 0 {
		t.Errorf("broken EXEC SQL produced statements: %v", facts.AllSQL)
	}
}

// TestUnbalancedBlockCommentFacts: a file ending inside a comment records
// the unterminated comment alongside the unclosed braces.
func TestUnbalancedBlockCommentFacts(t *testing.T) {
	src := "void F(void)\n{\n    /* never closed\n"
	facts, err := ScanBytes([]byte(src), "dangling.pc")
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(facts.Unbalanced) != 2 { // block comment + braces
		t.Fatalf("unbalanced = %v, want block_comment + braces", facts.Unbalanced)
	}
	if facts.Unbalanced[0].Kind != "block_comment" || facts.Unbalanced[0].StartLine != 3 {
		t.Errorf("block comment fact = %+v", facts.Unbalanced[0])
	}
}

// TestScanFragmentLineRebase pins PF-3.2: the __fragment wrapper shifts out,
// every line number is the fragment file's own, and the synthesis is marked.
func TestScanFragmentLineRebase(t *testing.T) {
	src := "char c_flag;\n\nif(c_flag == 'H')\n{\n    helper(c_flag);\n}\n"
	facts, err := ScanFragment([]byte(src), "slice.txt")
	if err != nil {
		t.Fatalf("ScanFragment failed: %v", err)
	}
	if !facts.Fragment {
		t.Error("facts.Fragment not set")
	}
	if facts.NumLines != 6 {
		t.Errorf("num lines = %d, want 6 (original fragment lines)", facts.NumLines)
	}
	if len(facts.Functions) != 1 || facts.Functions[0].Name != "__fragment" {
		t.Fatalf("functions = %v, want the __fragment synthesis", facts.Functions)
	}
	if fn := facts.Functions[0]; fn.StartLine != 1 || fn.BodyStartLine != 1 || fn.BodyEndLine != 6 {
		t.Errorf("__fragment extent = %+v", fn)
	}
	// Branch and call lines are the fragment's own (if at 3, call at 5).
	if len(facts.Branches) != 1 || facts.Branches[0].StartLine != 3 || facts.Branches[0].Function != "__fragment" {
		t.Errorf("branch = %+v, want if at line 3 owned by __fragment", facts.Branches)
	}
	if len(facts.Calls) != 1 || facts.Calls[0].Line != 5 {
		t.Errorf("call = %+v, want helper at line 5", facts.Calls)
	}
	if len(facts.VarDecls) != 1 || facts.VarDecls[0].Line != 1 {
		t.Errorf("var decl = %+v, want c_flag at line 1", facts.VarDecls)
	}
}

func queriesOf(facts *SourceFacts) []string {
	var out []string
	for _, q := range facts.Queries {
		out = append(out, q.Normalized)
	}
	return out
}

// TestCommentColumnExtent sanity: same-line comment extents carry both ends
// and the trailing comment hides nothing.
func TestCommentColumnExtent(t *testing.T) {
	src := "int x; /* trailing { } */ x = 2;\n"
	facts, err := ScanBytes([]byte(src), "cols.pc")
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if len(facts.Comments) != 1 {
		t.Fatalf("comments = %+v", facts.Comments)
	}
	c := facts.Comments[0]
	if c.StartLine != 1 || c.EndLine != 1 || c.EndCol <= c.StartCol {
		t.Errorf("same-line comment extent = %+v", c)
	}
	if len(facts.Queries) != 0 {
		t.Errorf("queries = %v, want none", facts.Queries)
	}
	if facts.InComment(1, 5) {
		t.Error("col 5 (int x) reported inside the trailing comment")
	}
}
