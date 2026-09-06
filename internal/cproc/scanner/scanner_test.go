package scanner

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestScannerNavFixture(t *testing.T) {
	pcPath := filepath.Join("..", "..", "..", "testdata", "nav", "SVC_MF_NAV_LIST.pc")
	facts, err := ScanFile(pcPath)
	if err != nil {
		t.Fatalf("failed scanning SVC_MF_NAV_LIST.pc: %v", err)
	}

	// 1. Assert exactly 7 live database queries
	if len(facts.Queries) != 7 {
		t.Errorf("expected 7 queries, got %d", len(facts.Queries))
	}
	for i, q := range facts.Queries {
		t.Logf("Query #%d (line %d-%d): [%s] %s", i+1, q.StartLine, q.EndLine, q.Kind, q.Normalized)
	}

	// 2. Assert tpcall count is 0
	if facts.TpCallCount != 0 {
		t.Errorf("expected 0 tpcall, got %d", facts.TpCallCount)
	}

	// 3. Verify function calls: chk_sssn, fn_is_d2u_active and fn_long_to_int are detected.
	// fn_long_to_int is called in LIVE code (the "Ver 1.5 added here for IBM"
	// region is delimited by banner comments, not wrapped in a C comment).
	var foundChkSssn, foundFnD2u, foundFnLongToInt bool
	for _, call := range facts.Calls {
		if call.Name == "chk_sssn" {
			foundChkSssn = true
		}
		if call.Name == "fn_is_d2u_active" {
			foundFnD2u = true
		}
		if call.Name == "fn_long_to_int" {
			foundFnLongToInt = true
		}
	}

	if !foundChkSssn {
		t.Errorf("expected chk_sssn to be detected as a function call")
	}
	if !foundFnD2u {
		t.Errorf("expected fn_is_d2u_active to be detected as a function call")
	}
	if !foundFnLongToInt {
		t.Errorf("expected fn_long_to_int to be detected as a function call (live code between version banners)")
	}

	// 4. Verify local function definition SVC_MF_NAV_LIST
	var foundSvcFunc bool
	for _, fn := range facts.Functions {
		if fn.Name == "SVC_MF_NAV_LIST" {
			foundSvcFunc = true
		}
	}
	if !foundSvcFunc {
		t.Errorf("expected function definition SVC_MF_NAV_LIST to be found")
	}

	// 5. Directives: system headers are recorded (Phase 2 IR input).
	var foundAtmi bool
	for _, d := range facts.Directives {
		if d.Kind == "include" && d.Arg == "<atmi.h>" && d.IsHeader && d.IsSystem {
			foundAtmi = true
		}
	}
	if !foundAtmi {
		t.Errorf("expected #include <atmi.h> to be recorded as a system header directive")
	}

	// 6. AllSQL carries every EXEC SQL statement, including non-query plumbing
	// (declare sections, Pro*C table includes) — Queries keeps only the
	// logical DB queries.
	var foundDeclareSection, foundTableInclude bool
	for _, s := range facts.AllSQL {
		if s.Kind == SQLDeclareSection {
			foundDeclareSection = true
		}
		if s.Kind == SQLInclude && strings.Contains(s.Normalized, "mf_navs.h") {
			foundTableInclude = true
		}
	}
	if !foundDeclareSection {
		t.Errorf("expected EXEC SQL BEGIN/END DECLARE SECTION statements in AllSQL")
	}
	if !foundTableInclude {
		t.Errorf(`expected EXEC SQL include "table/mf_navs.h" in AllSQL`)
	}
	if len(facts.AllSQL) <= len(facts.Queries) {
		t.Errorf("AllSQL must strictly contain Queries plus non-query statements (all=%d queries=%d)", len(facts.AllSQL), len(facts.Queries))
	}
}

func TestScannerFnD2uFixture(t *testing.T) {
	pcPath := filepath.Join("..", "..", "..", "testdata", "nav", "fn_d2u_mf.pc")
	facts, err := ScanFile(pcPath)
	if err != nil {
		t.Fatalf("failed scanning fn_d2u_mf.pc: %v", err)
	}

	// 1. Assert exactly 1 query
	if len(facts.Queries) != 1 {
		t.Errorf("expected 1 query, got %d", len(facts.Queries))
		for i, q := range facts.Queries {
			t.Logf("Query #%d: [%s] %s", i+1, q.Kind, q.Normalized)
		}
	}

	// 2. Assert 0 tpcall
	if facts.TpCallCount != 0 {
		t.Errorf("expected 0 tpcall, got %d", facts.TpCallCount)
	}

	// 3. Assert local function fn_is_d2u_active is defined
	var foundFnDef bool
	for _, fn := range facts.Functions {
		if fn.Name == "fn_is_d2u_active" {
			foundFnDef = true
		}
	}
	if !foundFnDef {
		t.Errorf("expected local function fn_is_d2u_active to be defined")
	}
}
