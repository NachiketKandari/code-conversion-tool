package sqlchk

import (
	"strings"
	"testing"
)

// TestCompareFidelity is the PF-6 gate: alias renames and bind-style
// changes pass; every real drift (columns, tables, conditions, set/values,
// order, binds) surfaces as exactly its typed deviation.
func TestCompareFidelity(t *testing.T) {
	cases := []struct {
		name     string
		source   string
		gen      string
		wantNone bool
		wantKind DeviationKind
	}{
		{"identical", "SELECT A, B FROM T WHERE C = :1", "SELECT A, B FROM T WHERE C = :1", true, ""},
		{"case and whitespace", "SELECT COMP_CD, SCH_CD FROM DEMO_PRICE WHERE STAT = 'A'",
			"select comp_cd, sch_cd\n  from demo_price\n where stat = 'A'", true, ""},
		{"trailing semicolon", "SELECT A FROM T;", "SELECT A FROM T", true, ""},
		{"table alias rename", "SELECT p.COMP_CD FROM DEMO_PRICE p WHERE p.STAT = :sql_stat",
			"SELECT price.COMP_CD FROM DEMO_PRICE price WHERE price.STAT = :1", true, ""},
		{"alias with as and join", "SELECT p.C, s.N FROM DEMO_PRICE p JOIN DEMO_SCHEME s ON p.ID = s.ID WHERE p.STAT = 'A'",
			"SELECT pr.C, sc.N FROM DEMO_PRICE AS pr JOIN DEMO_SCHEME AS sc ON pr.ID = sc.ID WHERE pr.STAT = 'A'", true, ""},
		{"column alias rename", `SELECT COMP_CD AS "CompCd" FROM DEMO_PRICE`,
			`SELECT COMP_CD AS "Code" FROM DEMO_PRICE`, true, ""},
		{"bare column alias", "SELECT COMP_CD code FROM DEMO_PRICE",
			"SELECT COMP_CD cd FROM DEMO_PRICE", true, ""},
		{"into carried identically", "SELECT COUNT(*) INTO :cnt FROM DUAL",
			"SELECT COUNT(*) INTO :cnt FROM DUAL", true, ""},
		{"named to positional binds", "WHERE A = :sql_a AND B = :sql_b",
			"WHERE A = :1 AND B = :2", true, ""},
		{"dropped condition", "WHERE A = 1 AND B = 2", "WHERE A = 1", false, DevWhere},
		{"changed literal", "WHERE STAT = 'A'", "WHERE STAT = 'N'", false, DevWhere},
		{"where added", "FROM T", "FROM T WHERE X = 1", false, DevWhere},
		{"table swap", "FROM A, B", "FROM B, A", false, DevTables},
		{"table renamed", "SELECT X FROM DEMO_PRICE", "SELECT X FROM DEMO_PRICE_HIST", false, DevTables},
		{"column reorder", "SELECT A, B FROM T", "SELECT B, A FROM T", false, DevColumns},
		{"extra column", "SELECT A FROM T", "SELECT A, B FROM T", false, DevColumns},
		{"changed expression", "SELECT NVL(A, 1) FROM T", "SELECT NVL(A, 2) FROM T", false, DevColumns},
		{"order by changed", "SELECT A FROM T ORDER BY A", "SELECT A FROM T ORDER BY A, B", false, DevOrder},
		{"named bind swap", "WHERE F(:sql_a, :sql_b) = 1", "WHERE F(:sql_b, :sql_a) = 1", false, DevBinds},
		{"positional bind swap", "WHERE F(:1, :2) = 1", "WHERE F(:2, :1) = 1", false, DevBinds},
		{"set column changed", "UPDATE T SET A = :1, B = :2 WHERE ID = :3",
			"UPDATE T SET A = :1, C = :2 WHERE ID = :3", false, DevSet},
		{"set bind swap", "UPDATE T SET A = :x, B = :y WHERE ID = :z",
			"UPDATE T SET A = :y, B = :x WHERE ID = :z", false, DevBinds},
		{"insert column changed", "INSERT INTO T (A, B) VALUES (:1, :2)",
			"INSERT INTO T (A, C) VALUES (:1, :2)", false, DevValues},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			devs := Compare(tc.source, tc.gen)
			if tc.wantNone {
				if len(devs) != 0 {
					t.Fatalf("deviations = %+v, want none", devs)
				}
				return
			}
			if len(devs) != 1 {
				t.Fatalf("deviations = %+v, want exactly one", devs)
			}
			if devs[0].Kind != tc.wantKind {
				t.Errorf("kind = %s, want %s (detail %q)", devs[0].Kind, tc.wantKind, devs[0].Detail)
			}
			if devs[0].Detail == "" {
				t.Errorf("deviation %s carries no detail", devs[0].Kind)
			}
		})
	}
}

// TestCompareSnippetBounded keeps the diff detail human-sized.
func TestCompareSnippetBounded(t *testing.T) {
	long := "WHERE A = 1 AND B = 2 AND C = 3 AND D = 4 AND E = 5 AND F = 6 AND G = 7"
	devs := Compare(long, "WHERE A = 1 AND B = 2 AND C = 3 AND D = 4 AND E = 5 AND F = 6 AND G = 8")
	if len(devs) != 1 || devs[0].Kind != DevWhere {
		t.Fatalf("deviations = %+v, want one where detail", devs)
	}
	if strings.Count(devs[0].Detail, "AND") > 7 {
		t.Errorf("detail not bounded: %q", devs[0].Detail)
	}
}
