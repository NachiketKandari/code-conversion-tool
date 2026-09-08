package plan

import (
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
)

// TestPlanErrorNamesTruncation pins severity F3's plan-side wording: a
// mapping against a file the scanner could not fully close must explain the
// likely truncation, not emit a bare "inventory has 0 conditions".
func TestPlanErrorNamesTruncation(t *testing.T) {
	f, err := ir.ExtractFile("../../testdata/adversarial/ADV_UNBALANCED_BRACE.pc")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Conditions) != 0 {
		t.Fatalf("precondition: ADV_UNBALANCED_BRACE has %d conditions, want 0", len(f.Conditions))
	}
	if len(f.Unbalanced) == 0 {
		t.Fatal("precondition: ADV_UNBALANCED_BRACE carries no unbalanced fact")
	}
	m := &Mapping{
		Service:    "advnosemi",
		Module:     "mutual-fund-be/pkg/services/advnosemi",
		RouteGroup: "/advnosemi",
		Endpoints: []Endpoint{
			{Condition: 1, Name: "NoSemiAlpha", Route: "/advnosemi/alpha"},
		},
		DBMethods: map[string]MethodPin{},
	}
	_, err = Build(Options{Main: f, Mapping: m, Budget: budget.New(12000, 4000, 4)})
	if err == nil {
		t.Fatal("plan.Build succeeded on a 0-condition inventory with a mapped endpoint")
	}
	if !strings.Contains(err.Error(), "unbalanced region") || !strings.Contains(err.Error(), "braces") {
		t.Errorf("plan error must name the unbalanced truncation, got: %v", err)
	}
}
