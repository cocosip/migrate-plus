package dm

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	databasetesting "github.com/golang-migrate/migrate/v4/database/testing"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// These tests destroy supported objects in the explicitly selected test schema.
func TestIntegration(t *testing.T) {
	raw := os.Getenv("MIGRATE_PLUS_DM_TEST_URL")
	if raw == "" {
		t.Skip("set MIGRATE_PLUS_DM_TEST_URL to a disposable DM8 schema; tests destroy its objects")
	}
	t.Run("upstream conformance", func(t *testing.T) { integrationConformance(t, raw) })
	t.Run("native Drop dependencies", func(t *testing.T) { integrationDrop(t, raw) })
	t.Run("session lock survives transactions and DDL", func(t *testing.T) { integrationSessionLock(t, raw) })
	t.Run("DML remains durable after SetVersion commit and rollback", func(t *testing.T) { integrationDurability(t, raw) })
	t.Run("upstream dirty force and down", func(t *testing.T) { integrationDirtyRecovery(t, raw) })
	t.Run("timeout terminates locked session", func(t *testing.T) { integrationTimeout(t, raw) })
	t.Run("cross schema dependency prevents destructive Drop", func(t *testing.T) { integrationCrossSchema(t, raw) })
}

func integrationDrop(t *testing.T, raw string) {
	d := openTestDriver(t, raw)
	mustExec(t, d, `CREATE TABLE migrate_plus_parent(id BIGINT PRIMARY KEY);
CREATE TABLE migrate_plus_child(id BIGINT PRIMARY KEY, parent_id BIGINT,
CONSTRAINT migrate_plus_fk FOREIGN KEY(parent_id) REFERENCES migrate_plus_parent(id));
CREATE VIEW migrate_plus_view AS SELECT id FROM migrate_plus_parent;
CREATE SEQUENCE migrate_plus_drop_seq;
CREATE SYNONYM migrate_plus_syn FOR migrate_plus_parent;
CREATE OR REPLACE FUNCTION migrate_plus_fn RETURN INT AS BEGIN RETURN 1; END;
/
`)
	if os.Getenv("MIGRATE_PLUS_DM_TEST_MATERIALIZED") == "1" {
		mustExec(t, d, `CREATE MATERIALIZED VIEW migrate_plus_mv AS SELECT id FROM migrate_plus_parent;
CREATE TABLE migrate_plus_prebuilt(id BIGINT);
CREATE MATERIALIZED VIEW migrate_plus_prebuilt_mv FOR migrate_plus_prebuilt ON PREBUILT TABLE AS SELECT id FROM migrate_plus_parent;`)
	}
	if err := d.Drop(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.query(`SELECT COUNT(*) FROM SYS.SYSOBJECTS WHERE SCHID = ? AND TYPE$ = 'SCHOBJ' AND NAME LIKE 'MIGRATE_PLUS_%'`, func(rows *sql.Rows) error {
		if !rows.Next() {
			return sql.ErrNoRows
		}
		return rows.Scan(&count)
	}, d.schemaID); err != nil || count != 0 {
		t.Fatalf("Drop left %d test objects: %v", count, err)
	}
}

func integrationConformance(t *testing.T, raw string) {
	d := openTestDriver(t, raw)
	databasetesting.Test(t, d, []byte("CREATE TABLE migrate_plus_probe (id BIGINT);"))
}

func integrationSessionLock(t *testing.T, raw string) {
	first, second := openTestDriver(t, raw), openTestDriver(t, raw)
	mustExec(t, first, "CREATE TABLE migrate_plus_lock_probe(id BIGINT)")
	if err := first.Lock(); err != nil {
		t.Fatal(err)
	}
	assertLocked(t, second)
	for _, commit := range []bool{true, false} {
		tx, err := first.conn.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(context.Background(), "INSERT INTO migrate_plus_lock_probe VALUES(1)"); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		assertLocked(t, second)
	}
	mustExec(t, first, "CREATE TABLE migrate_plus_ddl_probe(id BIGINT)")
	assertLocked(t, second)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Lock(); err != nil {
		t.Fatalf("close retained session lock: %v", err)
	}
	if err := second.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationDurability(t *testing.T, raw string) {
	first, observer := openTestDriver(t, raw), openTestDriver(t, raw)
	mustExec(t, first, "CREATE TABLE migrate_plus_durable(id BIGINT)")
	for i := 1; i <= 2; i++ {
		if err := first.SetVersion(i, true); err != nil {
			t.Fatal(err)
		}
		mustExec(t, first, "INSERT INTO migrate_plus_durable VALUES(1)")
		if err := first.SetVersion(i, false); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := first.conn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(context.Background(), "INSERT INTO migrate_plus_durable VALUES(99)"); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, first, "INSERT INTO migrate_plus_durable VALUES(1)")
	assertCount(t, observer, quoteIdentifier(first.schema)+`."MIGRATE_PLUS_DURABLE"`, 3)
	if err := first.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationDirtyRecovery(t *testing.T, raw string) {
	d := openTestDriver(t, raw)
	source, err := iofs.New(fstest.MapFS{
		"1_init.up.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE migrate_plus_items(id BIGINT PRIMARY KEY);
CREATE SEQUENCE migrate_plus_seq;
CREATE OR REPLACE PROCEDURE migrate_plus_noop AS BEGIN NULL; END;
/
INSERT INTO migrate_plus_items VALUES(1);`)},
		"1_init.down.sql": &fstest.MapFile{Data: []byte(`DROP PROCEDURE migrate_plus_noop;
DROP SEQUENCE migrate_plus_seq; DROP TABLE migrate_plus_items RESTRICT;`)},
		"2_fail.up.sql":   &fstest.MapFile{Data: []byte("INSERT INTO migrate_plus_missing VALUES(1);")},
		"2_fail.down.sql": &fstest.MapFile{Data: []byte("BEGIN NULL; END;\n/\n")},
	}, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "dm", d)
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			t.Errorf("migrator close: %v, %v", srcErr, dbErr)
		}
	})
	if err := m.Steps(1); err != nil {
		t.Fatal(err)
	}
	if err := m.Steps(1); err == nil {
		t.Fatal("invalid migration succeeded")
	}
	persisted := openTestDriver(t, raw)
	if v, dirty, err := persisted.Version(); err != nil || v != 2 || !dirty {
		t.Fatalf("dirty state was not durable: %d %v %v", v, dirty, err)
	}
	var dirtyErr migrate.ErrDirty
	if err := m.Up(); !errors.As(err, &dirtyErr) {
		t.Fatalf("upstream accepted dirty state: %v", err)
	}
	if err := m.Force(1); err != nil {
		t.Fatal(err)
	}
	if err := m.Down(); err != nil {
		t.Fatal(err)
	}
	if v, dirty, err := d.Version(); err != nil || v != database.NilVersion || dirty {
		t.Fatalf("down state: %d %v %v", v, dirty, err)
	}
	if err := d.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationTimeout(t *testing.T, raw string) {
	first, second := openTestDriver(t, raw), openTestDriver(t, raw)
	if err := first.Lock(); err != nil {
		t.Fatal(err)
	}
	first.config.StatementTimeout = 20 * time.Millisecond
	if err := first.Run(strings.NewReader("BEGIN DBMS_LOCK.SLEEP(1); END;\n/")); err == nil {
		t.Fatal("sleep exceeded timeout without error")
	}
	if _, _, err := first.Version(); err == nil {
		t.Fatal("continued on a cancelled session")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := second.Lock()
		if err == nil {
			break
		}
		if !errors.Is(err, database.ErrLocked) || time.Now().After(deadline) {
			t.Fatalf("cancelled session retained lock: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := second.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationCrossSchema(t *testing.T, raw string) {
	otherURL := os.Getenv("MIGRATE_PLUS_DM_OTHER_TEST_URL")
	if otherURL == "" {
		t.Skip("set MIGRATE_PLUS_DM_OTHER_TEST_URL to another disposable schema on the same server with cross-schema SELECT privileges")
	}
	first, other := openTestDriver(t, raw), openTestDriver(t, otherURL)
	if first.schemaID == other.schemaID {
		t.Fatal("cross-schema test requires two distinct schemas")
	}
	mustExec(t, first, "CREATE TABLE migrate_plus_external(id BIGINT)")
	mustExec(t, first, "INSERT INTO migrate_plus_external VALUES(1)")
	mustExec(t, other, "CREATE TABLE migrate_plus_sentinel(id BIGINT)")
	mustExec(t, other, "INSERT INTO migrate_plus_sentinel VALUES(7)")
	mustExec(t, other, `CREATE VIEW migrate_plus_external_view AS SELECT id FROM `+quoteIdentifier(first.schema)+`."MIGRATE_PLUS_EXTERNAL"`)
	t.Cleanup(func() {
		_ = other.exec("DROP VIEW IF EXISTS migrate_plus_external_view RESTRICT")
		_ = other.exec("DROP TABLE IF EXISTS migrate_plus_sentinel RESTRICT")
	})
	if err := first.Drop(); err == nil {
		t.Fatal("Drop accepted another schema's dependency")
	}
	assertCount(t, other, "migrate_plus_external_view", 1)
	assertCount(t, other, "migrate_plus_sentinel", 1)
	mustExec(t, other, "DROP VIEW migrate_plus_external_view RESTRICT")
	if err := first.Drop(); err != nil {
		t.Fatal(err)
	}
	assertCount(t, other, "migrate_plus_sentinel", 1)
}

func openTestDriver(t *testing.T, raw string) *Dm {
	t.Helper()
	driver, err := database.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := driver.(*Dm)
	t.Logf("mode=%d caseSensitive=%v backslash=%v ddlAutoCommit=%v protocol=%s", d.caps.mode, d.caps.caseSensitive, d.caps.backslash, d.ddlAutoCommit, d.protocolVersion)
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d
}

func mustExec(t *testing.T, d *Dm, script string) {
	t.Helper()
	if err := d.Run(strings.NewReader(script)); err != nil {
		t.Fatal(err)
	}
}

func assertLocked(t *testing.T, d *Dm) {
	t.Helper()
	if err := d.Lock(); !errors.Is(err, database.ErrLocked) {
		t.Fatalf("independent session entered migration: %v", err)
	}
}

func assertCount(t *testing.T, d *Dm, table string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := d.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != want {
		t.Fatalf("row count: %d want %d: %v", count, want, err)
	}
}
