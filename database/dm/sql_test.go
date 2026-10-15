package dm

import (
	"reflect"
	"testing"
)

func TestStatementFramingInEveryDMMode(t *testing.T) {
	for _, mode := range []int{0, 2, 4, 7} {
		cases := []struct {
			script string
			want   []string
		}{
			{"-- empty;\n/* / */", nil},
			{`INSERT INTO "a;b" VALUES ('x;''y'); UPDATE t SET v='next'`, []string{`INSERT INTO "a;b" VALUES ('x;''y')`, `UPDATE t SET v='next'`}},
			{"BEGIN\n NULL;\nEND;\n/\nCREATE TABLE t (id INT);", []string{"BEGIN\n NULL;\nEND;", "CREATE TABLE t (id INT)"}},
			{"DECLARE x INT; BEGIN x := 1; END;", []string{"DECLARE x INT; BEGIN x := 1; END;"}},
			{"<<label>> BEGIN NULL; END;\r\n / \r\n", []string{"<<label>> BEGIN NULL; END;"}},
			{"CREATE OR REPLACE PROCEDURE p AS BEGIN NULL; END;\n/\nCREATE FUNCTION f RETURN INT AS BEGIN RETURN 1; END;", []string{"CREATE OR REPLACE PROCEDURE p AS BEGIN NULL; END;", "CREATE FUNCTION f RETURN INT AS BEGIN RETURN 1; END;"}},
			{"CREATE TYPE ty AS OBJECT (id INT);\n/\n", []string{"CREATE TYPE ty AS OBJECT (id INT);"}},
			{"CREATE CLASS cls AS x INT; MEMBER FUNCTION get_x RETURN INT; END;\n/\nSELECT 1;", []string{"CREATE CLASS cls AS x INT; MEMBER FUNCTION get_x RETURN INT; END;", "SELECT 1"}},
			{"CREATE OR REPLACE CLASS BODY cls AS MEMBER FUNCTION get_x RETURN INT AS BEGIN RETURN x; END; END;\n/", []string{"CREATE OR REPLACE CLASS BODY cls AS MEMBER FUNCTION get_x RETURN INT AS BEGIN RETURN x; END; END;"}},
			{"INSERT INTO t VALUES ('a\n/\nb');\n/\n", []string{"INSERT INTO t VALUES ('a\n/\nb')"}},
			{"/* ; / */ CREATE TABLE t(id INT); -- tail", []string{"/* ; / */ CREATE TABLE t(id INT)"}},
		}
		for _, tc := range cases {
			got, err := splitStatements(tc.script, capabilities{mode: mode})
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mode %d script %q: %#v, %v; want %#v", mode, tc.script, got, err, tc.want)
			}
		}
	}
}

func TestFramingUsesActualBackslashSetting(t *testing.T) {
	script := `INSERT INTO t VALUES ('it\'s;safe'); DELETE FROM t;`
	want := []string{`INSERT INTO t VALUES ('it\'s;safe')`, `DELETE FROM t`}
	for _, mode := range []int{0, 2, 4, 7} {
		got, err := splitStatements(script, capabilities{mode: mode, backslash: true})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("mode %d: %q %v", mode, got, err)
		}
		if _, err := splitStatements(script, capabilities{mode: mode}); err == nil {
			t.Fatalf("mode %d guessed escaping from mode", mode)
		}
	}
	got, err := splitStatements(`SELECT 'c:\path'; SELECT 2`, capabilities{})
	if err != nil || len(got) != 2 {
		t.Fatalf("literal backslash: %q %v", got, err)
	}
}

func TestFramingRejectsUnsupportedOrIncompleteInputBeforeExecution(t *testing.T) {
	for _, script := range []string{
		`SELECT 'unterminated`, `SELECT "unterminated`, `SELECT 1; /* open`,
		"BEGIN NULL;", "BEGIN NULL; END", "<<bad BEGIN NULL; END;", "/* nested /* comment */",
		"DELIMITER $$\nCREATE PROCEDURE p() BEGIN SELECT 1; END$$", "SOURCE input.sql", "\\i input.sql", "@input.sql",
		"SET SERVEROUTPUT ON", "SET DEFINE OFF", "SET SCHEMA OTHER", "PROMPT hello", "SPOOL out.log", "EXIT", "QUIT",
		"COPY items FROM STDIN;\n1\n\\.", "SELECT `a;b` FROM t", "SELECT q'[a;b]' FROM dual", "SELECT nq'{a;b}' FROM dual",
		"SELECT $$a;b$$", "CREATE FUNCTION f() RETURNS VOID AS $body$ BEGIN NULL; END; $body$ LANGUAGE plpgsql;",
		"WITH FUNCTION f RETURN INT AS BEGIN RETURN 1; END; SELECT f FROM dual;", "BEGIN TRANSACTION; INSERT INTO t VALUES(1); COMMIT;",
	} {
		if got, err := splitStatements(script, capabilities{}); err == nil || got != nil {
			t.Errorf("unsafe input accepted: %q: %q %v", script, got, err)
		}
	}
}

func FuzzStatementFraming(f *testing.F) {
	for _, s := range []string{"", "SELECT 'a;b';", "BEGIN NULL; END;\n/", "/* broken", "SELECT \\"} {
		f.Add(s, false)
	}
	f.Fuzz(func(_ *testing.T, script string, backslash bool) {
		_, _ = splitStatements(script, capabilities{backslash: backslash})
	})
}
