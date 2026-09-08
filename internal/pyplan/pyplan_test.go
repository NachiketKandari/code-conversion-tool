package pyplan

import "testing"

func TestBindIdxOrdering(t *testing.T) {
	got := bindIdx([]string{"b", "a"}, []string{"a", "b"})
	if len(got) != 2 || got[0] != 1 || got[1] != 0 {
		t.Errorf("bindIdx = %v, want [1 0]", got)
	}
}

func TestCamel(t *testing.T) {
	if got := Camel("bat_mf_mbm_rt"); got != "BatMfMbmRt" {
		t.Errorf("Camel = %q", got)
	}
	if got := Camel("demo@weird_name"); got != "DemoWeirdName" {
		t.Errorf("Camel sanitization = %q", got)
	}
}

func TestBindOrder(t *testing.T) {
	sql := "UPDATE T SET A = :x_a, B = :x_b WHERE C = : x_c AND D = :x_a"
	got := bindOrder(sql)
	if len(got) != 3 || got[0] != "x_a" || got[1] != "x_b" || got[2] != "x_c" {
		t.Errorf("bindOrder = %v, want [x_a x_b x_c] (dedup + `: name` collapse)", got)
	}
}

func TestStripInto(t *testing.T) {
	sql := "SELECT A, B INTO :h_a, :h_b FROM DEMO_T WHERE A = :bind_a"
	got := stripInto(sql)
	if got != "SELECT A, B FROM DEMO_T WHERE A = :bind_a" {
		t.Errorf("stripInto = %q", got)
	}
	insert := "INSERT INTO DEMO_T (A) (SELECT A FROM OTHER_T)"
	if got := stripInto(insert); got != insert {
		t.Errorf("stripInto rewrote an INSERT: %q", got)
	}
}

func TestCollapseBinds(t *testing.T) {
	got := collapseBinds("UPDATE T SET A = : x_a WHERE B =:x_b AND C = 'x: y'")
	want := "UPDATE T SET A = :x_a WHERE B =:x_b AND C = 'x: y'"
	if got != want {
		t.Errorf("collapseBinds = %q, want %q", got, want)
	}
}
