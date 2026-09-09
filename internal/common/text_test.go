package common

import "testing"

func TestLeading(t *testing.T) {
	cases := []struct{ in, want string }{
		{"    x", "    "},
		{"\t\ty", "\t\t"},
		{"no-indent", ""},
		{"", ""},
		{"   ", "   "},
	}
	for _, c := range cases {
		if got := Leading(c.in); got != c.want {
			t.Errorf("Leading(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestUniqueStable(t *testing.T) {
	got := UniqueStable([]string{"b", "a", "b", "c", "a", ""})
	want := []string{"b", "a", "c", ""}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if out := UniqueStable(nil); out == nil || len(out) != 0 {
		t.Fatalf("nil input must yield an empty non-nil-safe result: %v", out)
	}
}

func TestNormalizeWS(t *testing.T) {
	if got := NormalizeWS("  a\t b\nc  "); got != "a b c" {
		t.Errorf("got %q", got)
	}
	if got := NormalizeWS(""); got != "" {
		t.Errorf("got %q", got)
	}
}
