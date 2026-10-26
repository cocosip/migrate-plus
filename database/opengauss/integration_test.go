//go:build integration

package opengauss

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

// These tests destroy supported objects in the explicitly selected disposable schema.
func TestIntegration(t *testing.T) {
	raw := os.Getenv("MIGRATE_PLUS_OPENGAUSS_TEST_URL")
	if raw == "" {
		t.Skip("set MIGRATE_PLUS_OPENGAUSS_TEST_URL to a disposable openGauss schema; tests destroy its objects")
	}
	t.Run("upstream conformance", func(t *testing.T) {
		databasetesting.Test(t, openTestDriver(t, raw), []byte("CREATE TABLE migrate_plus_probe(id BIGINT);"))
	})
	t.Run("native Drop dependencies", func(t *testing.T) { integrationDrop(t, raw) })
	t.Run("session lock survives transactions and DDL", func(t *testing.T) { integrationSessionLock(t, raw) })
	t.Run("durable state and defensive rollback recovery", func(t *testing.T) { integrationDurability(t, raw) })
	t.Run("upstream dirty force and down", func(t *testing.T) { integrationDirtyRecovery(t, raw) })
	t.Run("timeout discards session", func(t *testing.T) { integrationTimeout(t, raw) })
	t.Run("cross schema dependency prevents Drop", func(t *testing.T) { integrationCrossSchema(t, raw) })
}

func integrationDrop(t *testing.T, raw string) {
	g := openTestDriver(t, raw)
	mustExec(t, g, `CREATE TABLE migrate_plus_parent(id BIGINT PRIMARY KEY);
CREATE TABLE migrate_plus_child(id BIGINT PRIMARY KEY, parent_id BIGINT,
CONSTRAINT migrate_plus_fk FOREIGN KEY(parent_id) REFERENCES migrate_plus_parent(id));
CREATE VIEW migrate_plus_view AS SELECT id FROM migrate_plus_parent;
CREATE SEQUENCE migrate_plus_seq OWNED BY migrate_plus_parent.id;
ALTER TABLE migrate_plus_parent ALTER COLUMN id SET DEFAULT nextval('migrate_plus_seq');
CREATE TYPE migrate_plus_pair AS (a integer, b integer);
CREATE FUNCTION migrate_plus_fn(integer) RETURNS integer AS $$ SELECT $1 $$ LANGUAGE SQL;
CREATE FUNCTION migrate_plus_fn(bigint) RETURNS bigint AS $$ SELECT $1 $$ LANGUAGE SQL;`)
	if err := g.Drop(); err != nil {
		t.Fatal(err)
	}
	objects, err := g.dropObjects()
	if err != nil || len(objects) != 0 {
		t.Fatalf("remaining objects = %v: %v", objects, err)
	}
}

func integrationSessionLock(t *testing.T, raw string) {
	first, second := openTestDriver(t, raw), openTestDriver(t, raw)
	if err := first.Lock(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, first, "CREATE TABLE migrate_plus_lock_probe(id BIGINT)")
	assertLocked(t, second)
	for _, end := range []string{"COMMIT", rollbackSQL} {
		mustExec(t, first, "BEGIN; INSERT INTO migrate_plus_lock_probe VALUES(1); "+end+";")
		assertLocked(t, second)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Lock(); err != nil {
		t.Fatalf("Close retained session lock: %v", err)
	}
	if err := second.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationDurability(t *testing.T, raw string) {
	first, observer := openTestDriver(t, raw), openTestDriver(t, raw)
	mustExec(t, first, "CREATE TABLE migrate_plus_durable(id BIGINT)")
	if err := first.SetVersion(1, true); err != nil {
		t.Fatal(err)
	}
	mustExec(t, first, "INSERT INTO migrate_plus_durable VALUES(1)")
	if err := first.SetVersion(1, false); err != nil {
		t.Fatal(err)
	}
	// A script leaving a transaction open is cleaned by the defensive rollback; the uncommitted
	// row disappears and the reserved session stays usable for further migrations.
	if err := first.Run(strings.NewReader("BEGIN; INSERT INTO migrate_plus_durable VALUES(2);")); err == nil || !strings.Contains(err.Error(), "open transaction") {
		t.Fatalf("unfinished transaction = %v", err)
	}
	if err := first.Run(strings.NewReader("BEGIN; SELECT * FROM migrate_plus_missing;")); err == nil {
		t.Fatal("accepted a failed statement")
	}
	mustExec(t, first, "INSERT INTO migrate_plus_durable VALUES(3)")
	assertCount(t, observer, quoteQualified(first.schema, "migrate_plus_durable"), 2)
	if v, dirty, err := observer.Version(); err != nil || v != 1 || dirty {
		t.Fatalf("durable version = %d %v %v", v, dirty, err)
	}
	if err := first.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationDirtyRecovery(t *testing.T, raw string) {
	g := openTestDriver(t, raw)
	source, err := iofs.New(fstest.MapFS{
		"1_init.up.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE migrate_plus_items(id BIGINT PRIMARY KEY);
INSERT INTO migrate_plus_items VALUES(1);`)},
		"1_init.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE migrate_plus_items RESTRICT;")},
		"2_fail.up.sql":   &fstest.MapFile{Data: []byte("BEGIN; INSERT INTO migrate_plus_items VALUES(2);")},
		"2_fail.down.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	}, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "opengauss", g)
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
	if err := m.Steps(1); err == nil || !strings.Contains(err.Error(), "open transaction") {
		t.Fatalf("uncommitted migration = %v", err)
	}
	persisted := openTestDriver(t, raw)
	assertCount(t, persisted, quoteQualified(g.schema, "migrate_plus_items"), 1)
	if v, dirty, err := persisted.Version(); err != nil || v != 2 || !dirty {
		t.Fatalf("persisted dirty state = %d %v %v", v, dirty, err)
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
	if v, dirty, err := g.Version(); err != nil || v != database.NilVersion || dirty {
		t.Fatalf("down state = %d %v %v", v, dirty, err)
	}
	if err := g.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationTimeout(t *testing.T, raw string) {
	g, contender := openTestDriver(t, raw), openTestDriver(t, raw)
	if err := g.Lock(); err != nil {
		t.Fatal(err)
	}
	g.config.StatementTimeout = 20 * time.Millisecond
	if err := g.Run(strings.NewReader("SELECT pg_catalog.pg_sleep(1)")); err == nil || !g.broken.Load() {
		t.Fatalf("timeout did not discard session: %v", err)
	}
	contender.config.StatementTimeout = 5 * time.Second
	if err := contender.Lock(); err != nil {
		t.Fatalf("cancelled session retained lock: %v", err)
	}
	if err := contender.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationCrossSchema(t *testing.T, raw string) {
	otherURL := os.Getenv("MIGRATE_PLUS_OPENGAUSS_TEST_OTHER_URL")
	if otherURL == "" {
		t.Skip("set MIGRATE_PLUS_OPENGAUSS_TEST_OTHER_URL to another disposable schema in the same database")
	}
	first, other := openTestDriver(t, raw), openTestDriver(t, otherURL)
	if first.databaseName != other.databaseName || first.schemaID == other.schemaID {
		t.Fatal("cross-schema test requires distinct schemas in the same database")
	}
	mustExec(t, first, "CREATE TABLE migrate_plus_local(id BIGINT); INSERT INTO migrate_plus_local VALUES(1)")
	mustExec(t, other, "CREATE TABLE migrate_plus_sentinel(id BIGINT); INSERT INTO migrate_plus_sentinel VALUES(1)")
	mustExec(t, other, "CREATE VIEW migrate_plus_external AS SELECT id FROM "+quoteQualified(first.schema, "migrate_plus_local"))
	t.Cleanup(func() { mustExec(t, other, "DROP VIEW IF EXISTS migrate_plus_external RESTRICT") })
	if err := first.Drop(); err == nil {
		t.Fatal("Drop accepted an external dependent")
	}
	assertCount(t, first, "migrate_plus_local", 1)
	assertCount(t, other, "migrate_plus_sentinel", 1)
	mustExec(t, other, "DROP VIEW migrate_plus_external RESTRICT")
	if err := first.Drop(); err != nil {
		t.Fatal(err)
	}
	assertCount(t, other, "migrate_plus_sentinel", 1)
	mustExec(t, other, "DROP TABLE migrate_plus_sentinel RESTRICT")
}

func openTestDriver(t *testing.T, raw string) *OpenGauss {
	t.Helper()
	d, err := database.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	g := d.(*OpenGauss)
	t.Logf("database=%s schema=%s compatibility=%s standardStrings=%v", g.databaseName, g.schema, g.caps.mode, g.caps.standardStrings)
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Error(err)
		}
	})
	return g
}

func mustExec(t *testing.T, g *OpenGauss, script string) {
	t.Helper()
	if err := g.Run(strings.NewReader(script)); err != nil {
		t.Fatal(err)
	}
}

func assertLocked(t *testing.T, g *OpenGauss) {
	t.Helper()
	var acquired bool
	if err := g.query(`SELECT pg_catalog.pg_try_advisory_lock($1::bigint)`, func(rows *sql.Rows) error {
		if !rows.Next() {
			return sql.ErrNoRows
		}
		return rows.Scan(&acquired)
	}, g.lockID); err != nil {
		t.Fatal(err)
	}
	if acquired {
		_ = g.exec(`SELECT pg_catalog.pg_advisory_unlock($1::bigint)`, g.lockID)
		t.Fatal("another session acquired the migration lock")
	}
}

func assertCount(t *testing.T, g *OpenGauss, table string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	if err := g.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != want {
		t.Fatalf("row count = %d, want %d: %v", count, want, err)
	}
}
