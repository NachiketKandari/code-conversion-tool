package ledger

import (
	"path/filepath"
	"testing"
)

func TestLedgerRoundTripAndResume(t *testing.T) {
	dir := t.TempDir()
	l, err := Load(dir, "nav")
	if err != nil {
		t.Fatal(err)
	}
	e := l.Get("u02", "db_method", "GetNavHistory")
	if e.Status != StatusPlanned {
		t.Errorf("fresh entry = %s, want planned", e.Status)
	}
	l.Set("u02", StatusValidated, "")
	l.Set("u02", StatusAppended, "", "pkg/services/nav/db/nav.go")
	l.AddMap("SVC_DEMO_LIST.pc :: GetNavHistory (L232-343)", "pkg/services/nav/db/nav.go")
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}

	l2, err := Load(dir, "nav")
	if err != nil {
		t.Fatal(err)
	}
	got := l2.Units["u02"]
	if got == nil || got.Status != StatusAppended || got.Name != "GetNavHistory" || len(got.Targets) != 1 {
		t.Errorf("round-tripped entry = %+v", got)
	}
	if len(l2.Map) != 1 || l2.Map[0].Target != "pkg/services/nav/db/nav.go" {
		t.Errorf("round-tripped map = %+v", l2.Map)
	}
	appended, failed, blocked, skipped, placeholders, deviated := l2.Counts()
	if appended != 1 || failed != 0 || blocked != 0 || skipped != 0 || placeholders != 0 || deviated != 0 {
		t.Errorf("counts = %d/%d/%d/%d/%d/%d", appended, failed, blocked, skipped, placeholders, deviated)
	}
}

func TestLedgerSetCreatesEntryWithStatus(t *testing.T) {
	l, err := Load(filepath.Join(t.TempDir(), "ledger"), "nav")
	if err != nil {
		t.Fatal(err)
	}
	l.Set("u99", StatusBlocked, "unresolved fn")
	e := l.Units["u99"]
	if e == nil || e.Status != StatusBlocked || e.Error != "unresolved fn" {
		t.Errorf("set-created entry = %+v", e)
	}
}
