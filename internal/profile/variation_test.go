package profile

import "testing"

// gormalt is the synthetic second Go profile (P2 variation proof, AD14):
// a different project's conventions — gorm-shaped store, different layer
// folder names, plural receiver policy — rendered over the same plan units
// gen/convert hold. If this profile cannot vary the shape without touching
// the pipeline, the seam is wrong and no third target starts.
type gormalt struct{}

func (gormalt) ID() string { return "gormalt" }

func (gormalt) Naming() Naming {
	return Naming{
		Request:  func(endpoint string) string { return endpoint + "Input" },
		Response: func(endpoint string) string { return endpoint + "Output" },
		Row:      func(methodName string) string { return methodName + "Result" },
		Receiver: func(service string) string { return lower(service) },
	}
}

func (gormalt) Layout() Layout {
	return Layout{
		Layers: map[string]string{
			"db":         "repositories",
			"controller": "endpoints",
			"handler":    "transports",
			"models":     "entities",
		},
		ServiceDir: func(module, service string) string {
			return module + "/internal/app/" + lower(service)
		},
	}
}

func (gormalt) DB() DBRules {
	return DBRules{
		StoreReceiver: "r.repo.",
		TxVariants:    func(string) bool { return false }, // gorm wraps txs differently
	}
}

func TestVariationProofSecondGoProfile(t *testing.T) {
	p, err := For("gormalt")
	if err != nil {
		Register(gormalt{})
		p, err = For("gormalt")
		if err != nil {
			t.Fatal(err)
		}
	}
	if p.ID() != "gormalt" {
		t.Fatalf("id = %s", p.ID())
	}
	n := p.Naming()
	if got := n.Request("NavList"); got != "NavListInput" {
		t.Errorf("Request = %q", got)
	}
	if got := n.Row("GetNavDetails"); got != "GetNavDetailsResult" {
		t.Errorf("Row = %q", got)
	}
	l := p.Layout()
	if got := l.Folder("db"); got != "repositories" {
		t.Errorf("db folder = %q", got)
	}
	if got := l.ServiceDir("myapp", "Nav"); got != "myapp/internal/app/nav" {
		t.Errorf("ServiceDir = %q", got)
	}
	db := p.DB()
	if db.StoreReceiver != "r.repo." {
		t.Errorf("StoreReceiver = %q", db.StoreReceiver)
	}
	if db.TxVariants("INSERT") {
		t.Errorf("gorm profile renders no tx variants")
	}
}

// common2/lower/service are local helpers keeping the test self-contained.
func lower(s string) string {
	b := []byte(s)
	if len(b) > 0 && b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}
