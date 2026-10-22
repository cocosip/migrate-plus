package oceanbase

import (
	"strings"
	"testing"

	ob "github.com/cocosip/go-oceanbase"
)

func TestScriptFraming(t *testing.T) {
	script := "CREATE TABLE t(id NUMBER);\nBEGIN\n INSERT INTO t VALUES(1);\nEND;\n/\nINSERT INTO t VALUES(2);"
	statements, err := splitStatements(script, ob.ModeOracle, "")
	if err != nil || len(statements) != 3 || !strings.HasSuffix(statements[1], "END;") {
		t.Fatalf("statements=%q error=%v", statements, err)
	}
	for _, script := range []string{"INSERT INTO t VALUES(q'[a;b]');", "INSERT INTO t VALUES(nq'{a;b}');", "/* comment ; */ INSERT INTO t VALUES('it''s;ok');"} {
		if statements, err := splitStatements(script, ob.ModeOracle, ""); err != nil || len(statements) != 1 {
			t.Fatalf("%q: %q %v", script, statements, err)
		}
	}
	mysql := "CREATE TABLE t(id BIGINT); CREATE PROCEDURE p() BEGIN INSERT INTO t VALUES(1); END; CALL p();"
	if statements, err := splitStatements(mysql, ob.ModeMySQL, ""); err != nil || len(statements) != 1 || statements[0] != mysql {
		t.Fatalf("mysql=%q %v", statements, err)
	}
}

func TestScriptPreflight(t *testing.T) {
	for _, mode := range []ob.Mode{ob.ModeMySQL, ob.ModeOracle} {
		for _, script := range []string{"CREATE TABLE t(id INT); USE other;", "SET autocommit=0", "ALTER SESSION SET CURRENT_SCHEMA=OTHER", "CONNECT user", "SOURCE file.sql", "DELIMITER //", "COMMIT", "ROLLBACK", "SELECT 'unclosed", "/* unclosed", "/*!50000 SET autocommit=0 */", "/*M! SET autocommit=0 */"} {
			if _, err := splitStatements(script, mode, ""); err == nil {
				t.Errorf("mode %v accepted %q", mode, script)
			}
		}
	}
	for _, script := range []string{"SELECT `semi;colon` FROM t;", "SELECT 'a\\';b'; #comment\n SELECT 1;", "SELECT 1--not_a_comment; SELECT 2;"} {
		if _, err := splitStatements(script, ob.ModeMySQL, ""); err != nil {
			t.Errorf("%q: %v", script, err)
		}
	}
	if _, err := splitStatements("SELECT 'a\\'; SELECT 1;", ob.ModeMySQL, "NO_BACKSLASH_ESCAPES"); err != nil {
		t.Fatal(err)
	}
	if _, err := splitStatements("BEGIN INSERT INTO t VALUES(1); END", ob.ModeOracle, ""); err == nil {
		t.Fatal("accepted incomplete block")
	}
}

func TestANSIQuotesCannotHideSessionCommands(t *testing.T) {
	script := "SELECT \"a\\\"; USE other; -- \";"
	if _, err := splitStatements(script, ob.ModeMySQL, "ANSI_QUOTES"); err == nil {
		t.Fatal("ANSI identifier hid USE from preflight")
	}
}

func TestRejectUnverifiedInlineOracleRoutine(t *testing.T) {
	if _, err := splitStatements("WITH FUNCTION f RETURN NUMBER IS BEGIN RETURN 1; END; SELECT f() FROM dual;", ob.ModeOracle, ""); err == nil {
		t.Fatal("accepted unsupported inline routine framing")
	}
}
