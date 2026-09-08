package ir

import (
	"testing"
)

// TestUnbalancedRidesOnIR pins severity F3: unbalanced regions the scanner
// records (unterminated comment / unbalanced brace) must reach ir.File —
// loud in extract output, never a silent truncation. (The missing-semicolon
// case is NOT a fact: the scanner closes the block at the next semicolon
// leniently — documented residual, see docs/severity-analysis.md §7-F3.)
func TestUnbalancedRidesOnIR(t *testing.T) {
	f, err := ExtractFile("../../../testdata/adversarial/ADV_UNBALANCED_COMMENT.pc")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Unbalanced) == 0 {
		t.Fatal("ADV_UNBALANCED_COMMENT extracted with no unbalanced fact — the unterminated comment must be loud")
	}
	kinds := map[string]bool{}
	for _, u := range f.Unbalanced {
		kinds[u.Kind] = true
	}
	if !kinds["block_comment"] {
		t.Errorf("unbalanced kinds = %v, want block_comment", f.Unbalanced)
	}

	b, err := ExtractFile("../../../testdata/adversarial/ADV_UNBALANCED_BRACE.pc")
	if err != nil {
		t.Fatal(err)
	}
	kinds = map[string]bool{}
	for _, u := range b.Unbalanced {
		kinds[u.Kind] = true
	}
	if !kinds["braces"] {
		t.Errorf("ADV_UNBALANCED_BRACE unbalanced kinds = %v, want braces", b.Unbalanced)
	}

	// Balanced files stay silent: the nav fixture carries no unbalanced
	// regions (the golden pins hold).
	nav, err := ExtractFile("../../../testdata/nav/SVC_DEMO_LIST.pc")
	if err != nil {
		t.Fatal(err)
	}
	if len(nav.Unbalanced) != 0 {
		t.Errorf("nav unbalanced = %v, want none", nav.Unbalanced)
	}
}

// TestTPCallEmptyContractIsAmbiguous pins severity F5: identified buffers
// with zero FML ops on both sides degrade to an ambiguous fact, so the
// placeholder states the empty contract instead of understating it.
func TestTPCallEmptyContractIsAmbiguous(t *testing.T) {
	f, err := ExtractFile("../../../testdata/adversarial/ADV_TPCALL_NOFML.pc")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.TPCalls) != 1 {
		t.Fatalf("tpcalls = %d, want 1", len(f.TPCalls))
	}
	tp := f.TPCalls[0]
	if tp.SendBuffer == "" || tp.RecvBuffer == "" {
		t.Fatalf("buffers not identified: %+v", tp)
	}
	if len(tp.SendFML) != 0 || len(tp.RecvFML) != 0 {
		t.Fatalf("fixture drifted — contract is not empty: %+v", tp)
	}
	if !tp.Ambiguous {
		t.Error("empty-contract tpcall must be flagged ambiguous, got a silently empty contract")
	}
}
