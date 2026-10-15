package oracle

import (
	"reflect"
	"testing"
)

func TestSplitStatements(t *testing.T) {
	const tableDDL = "CREATE TABLE t (id NUMBER)"
	for _, tc := range []struct {
		name, script string
		want         []string
	}{
		{"empty", " \n;\n-- comment\n/* comment */\n/\n", nil},
		{"sql", tableDDL + ";\nINSERT INTO t VALUES (1);", []string{tableDDL, "INSERT INTO t VALUES (1)"}},
		{"unterminated sql", tableDDL, []string{tableDDL}},
		{"literals", `INSERT INTO "a;b" VALUES ('it''s;fine', q'[a';b]', nq'{x;y}', Q'!z;!');`, []string{`INSERT INTO "a;b" VALUES ('it''s;fine', q'[a';b]', nq'{x;y}', Q'!z;!')`}},
		{"comments", "-- lead;\nCREATE /* ; */ TABLE t (id NUMBER); -- tail;", []string{"-- lead;\nCREATE /* ; */ TABLE t (id NUMBER)"}},
		{"block", "BEGIN\n NULL;\n BEGIN NULL; END;\nEND;\n/\nSELECT 1 FROM dual;", []string{"BEGIN\n NULL;\n BEGIN NULL; END;\nEND;", "SELECT 1 FROM dual"}},
		{"declare eof", "DECLARE x NUMBER; BEGIN x := 1; END;", []string{"DECLARE x NUMBER; BEGIN x := 1; END;"}},
		{"procedure", "CREATE OR REPLACE PROCEDURE p AS BEGIN NULL; END;\n/\nDROP PROCEDURE p;", []string{"CREATE OR REPLACE PROCEDURE p AS BEGIN NULL; END;", "DROP PROCEDURE p"}},
		{"package", "CREATE OR REPLACE EDITIONABLE PACKAGE BODY p AS PROCEDURE f IS BEGIN NULL; END; END;\n/", []string{"CREATE OR REPLACE EDITIONABLE PACKAGE BODY p AS PROCEDURE f IS BEGIN NULL; END; END;"}},
		{"type", "CREATE TYPE t AS OBJECT (id NUMBER);\n/", []string{"CREATE TYPE t AS OBJECT (id NUMBER);"}},
		{"slash in literals", "BEGIN x := q'[\n/\n;]'; END;\n/\n", []string{"BEGIN x := q'[\n/\n;]'; END;"}},
		{"slash in comments", "BEGIN /*\n/\n*/ NULL; END;\n/", []string{"BEGIN /*\n/\n*/ NULL; END;"}},
		{"crlf", "BEGIN\r\n NULL;\r\nEND;\r\n / \r\nSELECT 1 FROM dual;\r\n/", []string{"BEGIN\r\n NULL;\r\nEND;", "SELECT 1 FROM dual"}},
		{"division", "INSERT INTO t VALUES (4 / 2);", []string{"INSERT INTO t VALUES (4 / 2)"}},
		{"escaped identifier", `CREATE TABLE "a"";b" (id NUMBER);`, []string{`CREATE TABLE "a"";b" (id NUMBER)`}},
		{"labelled block", "<<outer_block>>\nBEGIN NULL; END;\n/", []string{"<<outer_block>>\nBEGIN NULL; END;"}},
		{"multibyte q delimiter", "INSERT INTO t VALUES (q'§a';b§');", []string{"INSERT INTO t VALUES (q'§a';b§')"}},
		{"inline function SQL", "WITH FUNCTION f RETURN NUMBER IS BEGIN RETURN 1; END; SELECT f() FROM dual; -- tail\n/", []string{"WITH FUNCTION f RETURN NUMBER IS BEGIN RETURN 1; END; SELECT f() FROM dual -- tail"}},
		{"inline function boundary", "WITH FUNCTION f RETURN NUMBER IS BEGIN RETURN 1; END; SELECT f() FROM dual;\n/\nSELECT 2 FROM dual;", []string{"WITH FUNCTION f RETURN NUMBER IS BEGIN RETURN 1; END; SELECT f() FROM dual", "SELECT 2 FROM dual"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := splitStatements(tc.script)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("statements = %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestSplitStatementsRejectsUnclosedLiteralsAndComments(t *testing.T) {
	for _, script := range []string{"SELECT 'x", `SELECT "x`, "SELECT q'[x", "SELECT 1 /* x"} {
		if _, err := splitStatements(script); err == nil {
			t.Errorf("accepted malformed script %q", script)
		}
	}
}
