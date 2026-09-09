package profile

import "testing"

func TestForDefaultsToGonav(t *testing.T) {
	p, err := For("")
	if err != nil {
		t.Fatalf("For(empty): %v", err)
	}
	if p.ID() != "gonav" {
		t.Fatalf("default profile = %s, want gonav", p.ID())
	}
}

func TestUnknownProfileErrors(t *testing.T) {
	if _, err := For("nonexistent"); err == nil {
		t.Fatal("unknown profile must error")
	}
}

// TestGonavPinsTodayConventions pins profile #1 to the conventions P1 will
// extract from gen/convert — the byte-identity contract's reference values.
func TestGonavPinsTodayConventions(t *testing.T) {
	p, _ := For("")
	n := p.Naming()
	if got := n.Request("NavList"); got != "NavListRequest" {
		t.Errorf("Request = %q", got)
	}
	if got := n.Response("NavList"); got != "NavListResponse" {
		t.Errorf("Response = %q", got)
	}
	for method, want := range map[string]string{
		"GetNavDetails":     "NavDetails",
		"MergeDemoAccounts": "DemoAccounts", // A2.6's verb-strip contract
		"InsertDate":        "Date",
	} {
		if got := n.Row(method); got != want {
			t.Errorf("Row(%q) = %q, want %q", method, got, want)
		}
	}
	if got := n.Receiver("Nav"); got != "nav" {
		t.Errorf("Receiver = %q", got)
	}
	db := p.DB()
	if db.StoreReceiver != "s.store." {
		t.Errorf("StoreReceiver = %q", db.StoreReceiver)
	}
	for qt, want := range map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true, "MERGE": false, "SELECT_SINGLE": false} {
		if got := db.TxVariants(qt); got != want {
			t.Errorf("TxVariants(%q) = %v", qt, got)
		}
	}
	l := p.Layout()
	if got := l.ServiceDir("mutual-fund-be", "Nav"); got != "mutual-fund-be/pkg/services/nav" {
		t.Errorf("ServiceDir = %q", got)
	}
}
