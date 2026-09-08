package pychk

import (
	"strings"
	"testing"
)

func TestCheckClean(t *testing.T) {
	src := `import logging

logger = logging.getLogger("app.x")

CONST = """
SELECT 1 FROM DUAL
"""

class Svc:
    def __init__(self):
        self.n = 0

    def run(self, workers: int = 1) -> dict:
        try:
            for i in range(3):
                self.n += i
            return {"status": "SUCCESS", "n": self.n}
        except Exception as ex:
            logger.error("failed: %s", ex)
            raise
`
	if issues := Check(src); len(issues) != 0 {
		t.Errorf("clean module flagged: %v", issues)
	}
}

func TestCheckBroken(t *testing.T) {
	cases := map[string]string{
		"unterminated block":  "def f():\n    return 1\ndef g():",
		"bad dedent":          "def f():\n        return 1\n    return 2",
		"unbalanced brackets": "x = [1, 2\ny = 3",
		"unterminated string": "s = \"\"\"\nnever closed",
		"opener same indent":  "if x:\nreturn 1",
	}
	for name, src := range cases {
		if issues := Check(src); len(issues) == 0 {
			t.Errorf("%s: no issue flagged", name)
		}
	}
}

func TestSQLLiterals(t *testing.T) {
	src := "A_Q = \"\"\"\nSELECT 1\nFROM DUAL\n\"\"\"\nB_Q = \"\"\"SELECT 2 FROM T\"\"\"\n"
	lits := SQLLiterals(src)
	if len(lits) != 2 {
		t.Fatalf("literals = %d, want 2", len(lits))
	}
	if lits[0].Name != "A_Q" || !strings.Contains(lits[0].SQL, "FROM DUAL") {
		t.Errorf("literal 0 = %+v", lits[0])
	}
	if lits[1].Name != "B_Q" || !strings.Contains(lits[1].SQL, "SELECT 2") {
		t.Errorf("literal 1 = %+v", lits[1])
	}
}

func TestFidelity(t *testing.T) {
	const gen = "Q1 = \"\"\"\nSELECT A, B FROM DEMO_T WHERE A = :bind_a\n\"\"\"\n"
	targets := []FidelityTarget{{Const: "Q1", QueryID: "q1", Source: "SELECT A, B FROM DEMO_T WHERE A = :bind_a"}}
	res := Fidelity(targets, gen)
	if len(res) != 1 || res[0].Status != "match" {
		t.Fatalf("fidelity = %+v, want match", res)
	}
	// Dropped WHERE condition must flag (the one typed deviation family).
	targets[0].Source = "SELECT A, B FROM DEMO_T WHERE A = :bind_a AND B = 1"
	res = Fidelity(targets, gen)
	if len(res) != 1 || res[0].Status != "deviated" {
		t.Fatalf("fidelity = %+v, want deviated", res)
	}
	// A renamed bind is a real drift (PF-6.2), never tolerated.
	targets[0].Source = "SELECT A, B FROM DEMO_T WHERE A = :other_name"
	res = Fidelity(targets, gen)
	if len(res) != 1 || res[0].Status != "deviated" {
		t.Fatalf("fidelity = %+v, want deviated on bind rename", res)
	}
	// Pro*C INTO lists are tolerance, never a deviation (BP-8); the WHERE
	// bind keeps its spelling.
	targets[0].Source = "SELECT A, B INTO :h_a, :h_b FROM DEMO_T WHERE A = :bind_a"
	res = Fidelity(targets, gen)
	if len(res) != 1 || res[0].Status != "match" {
		t.Fatalf("fidelity = %+v, want match with INTO tolerance", res)
	}
	// Missing literal is unverifiable, never a silent pass.
	res = Fidelity(targets, "NOSUCH = \"\"\"x\"\"\"")
	if len(res) != 1 || res[0].Status != "unverifiable" {
		t.Fatalf("fidelity = %+v, want unverifiable", res)
	}
}
