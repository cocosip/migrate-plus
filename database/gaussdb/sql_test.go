package gaussdb

import "testing"

func TestScriptPreflightPreservesNativeSQL(t *testing.T) {
	for _, script := range []string{
		"", " -- comment\n /* nested /* comment */ ok */ ", "CREATE TABLE t (id bigint); INSERT INTO t VALUES (1);",
		`SELECT 'COPY t FROM STDIN; \\copy', "COPY", 'it''s fine';`,
		`DO $$ BEGIN RAISE NOTICE 'COPY t FROM STDIN;'; END $$;`,
		`CREATE FUNCTION f() RETURNS text AS $body$ BEGIN RETURN '\\copy;'; END; $body$ LANGUAGE plpgsql;`,
		`SELECT E'escaped\'; COPY text;'; SELECT 1;`,
		`SELECT q'[COPY t FROM STDIN; it''s fine]';`,
		"BEGIN; SELECT 1; COMMIT;", "BEGIN TRANSACTION; SELECT 1; ROLLBACK;",
		`SELECT (1 / 2); -- COPY t FROM STDIN`,
	} {
		if err := validateScript(script, capabilities{mode: "A", standardStrings: true}); err != nil {
			t.Errorf("rejected %q: %v", script, err)
		}
	}
}

func TestScriptPreflightRejectsProtocolAndClientCommandsBeforeExecution(t *testing.T) {
	for _, script := range []string{
		"COPY t FROM STDIN;\n1\n\\.", "SELECT 1; /* comment */ COPY t TO STDOUT;", "COPY t FROM '/server/file';",
		"-- comment\rCOPY t FROM STDIN;", "-- comment\r\nCOPY t FROM STDIN;",
		"SELECT '\x81\x5c'; COPY t FROM STDIN; --';",
		"SELECT '\x80';",
		"\\copy t from 'file'", "SELECT 1;\n\\i file.sql", "DELIMITER $$", "SOURCE file.sql", "SELECT 1;\n/\n",
		"SELECT 'unclosed", "SELECT $$unclosed", "SELECT /* unclosed", "SELECT q'[unclosed", "SELECT \x00",
		"DECLARE x int; BEGIN NULL; END;", "BEGIN NULL; END;", "CREATE PROCEDURE p() AS BEGIN NULL; END;",
	} {
		if err := validateScript(script, capabilities{mode: "A", standardStrings: true}); err == nil {
			t.Errorf("accepted unsafe script %q", script)
		}
	}
}

func TestScriptPreflightPreservesContinuedEscapeStrings(t *testing.T) {
	for _, mode := range []string{"A", "PG"} {
		caps := capabilities{mode: mode, standardStrings: true}
		for _, gap := range []string{"\n", "\r\n", " -- continuation\n", "\n -- continuation\n"} {
			script := "SELECT E'prefix'" + gap + "'\\''; COPY t FROM STDIN; --';"
			if err := validateScript(script, caps); err == nil {
				t.Errorf("accepted COPY after continued escape string in %s: %q", mode, script)
			}
			script = "SELECT E'prefix'" + gap + "'it\\'s still a string; COPY t FROM STDIN;';"
			if err := validateScript(script, caps); err != nil {
				t.Errorf("rejected valid continued escape string in %s: %v", mode, err)
			}
		}
	}
}

func TestScriptPreflightUsesEffectiveStringEscaping(t *testing.T) {
	script := `SELECT 'escaped\'; COPY hidden;'; SELECT 1;`
	if err := validateScript(script, capabilities{mode: "PG", standardStrings: false}); err != nil {
		t.Fatal(err)
	}
	if err := validateScript(script, capabilities{mode: "PG", standardStrings: true}); err == nil {
		t.Fatal("accepted COPY after a standard-conforming string")
	}
}

func FuzzScriptPreflight(f *testing.F) {
	for _, script := range []string{"SELECT 1;", "COPY t FROM STDIN;", "DO $$ BEGIN NULL; END $$;", "/* nested /* */ */", "q'[quote]'"} {
		f.Add(script, true)
	}
	f.Fuzz(func(_ *testing.T, script string, standardStrings bool) {
		_ = validateScript(script, capabilities{mode: "A", standardStrings: standardStrings})
	})
}
