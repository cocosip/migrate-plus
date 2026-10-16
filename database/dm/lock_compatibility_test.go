package dm_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	dmclient "gitee.com/chunanyong/dm"
)

const (
	probeBlockLiteral = "block-literal"
	probeBound        = "bound"
	probeBlock        = "block"
)

// TestLockCompatibility uses only database/sql and the native client. Each case
// owns a new pool/session, and stops at its first error. It never runs DDL/Drop.
func TestLockCompatibility(t *testing.T) {
	dsn := os.Getenv("MIGRATE_PLUS_DM_LOCK_PROBE_DSN")
	if dsn == "" {
		t.Skip("set MIGRATE_PLUS_DM_LOCK_PROBE_DSN for native lock diagnostics (no DDL/Drop)")
	}
	for _, tc := range []struct {
		name       string
		mode       string
		wait       int
		cancelable bool
		prepared   bool
	}{
		{"legacy_exact", "legacy", 60, false, false},
		{"select_literal_60", "literal", 60, false, false},
		{"block_literal_60", probeBlockLiteral, 60, false, false},
		{"select_bound_60", probeBound, 60, false, false},
		{"block_bound_60", probeBlock, 60, false, false},
		{"select_bound_0", probeBound, 0, false, false},
		{"block_bound_0", probeBlock, 0, false, false},
		{"select_bound_cancelable", probeBound, 0, true, false},
		{"block_bound_cancelable", probeBlock, 0, true, false},
		{"block_prepared_cancelable", probeBlock, 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := probeConnection(t, dsn)
			ctx := context.Background()
			if tc.cancelable {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 90*time.Second)
				defer cancel()
			}
			probeOne(ctx, t, conn, "before")
			request := fmt.Sprintf("SELECT DBMS_LOCK.REQUEST(483917264, 6, %d, FALSE) FROM DUAL", tc.wait)
			release := "SELECT DBMS_LOCK.RELEASE(483917264) FROM DUAL"
			var requestArgs, releaseArgs []any
			code := -1
			switch tc.mode {
			case "legacy":
				request = "SELECT DBMS_LOCK.REQUEST(483917264, DBMS_LOCK.X_MODE, 60, FALSE) FROM DUAL"
			case probeBound:
				request = fmt.Sprintf("SELECT DBMS_LOCK.REQUEST(?, 6, %d, FALSE) FROM DUAL", tc.wait)
				release = "SELECT DBMS_LOCK.RELEASE(?) FROM DUAL"
				requestArgs, releaseArgs = []any{int64(483917264)}, []any{int64(483917264)}
			case probeBlockLiteral:
				request = fmt.Sprintf("BEGIN ? := DBMS_LOCK.REQUEST(483917264, 6, %d, FALSE); END;", tc.wait)
				release = "BEGIN ? := DBMS_LOCK.RELEASE(483917264); END;"
				requestArgs, releaseArgs = []any{sql.Out{Dest: &code}}, []any{sql.Out{Dest: &code}}
			case probeBlock:
				request = fmt.Sprintf("BEGIN ? := DBMS_LOCK.REQUEST(?, 6, %d, FALSE); END;", tc.wait)
				release = "BEGIN ? := DBMS_LOCK.RELEASE(?); END;"
				requestArgs = []any{sql.Out{Dest: &code}, int64(483917264)}
				releaseArgs = []any{sql.Out{Dest: &code}, int64(483917264)}
			}
			for i, query := range []string{request, release} {
				stage, args := "request", requestArgs
				if i == 1 {
					stage, args = "release", releaseArgs
				}
				code = -1
				t.Logf("%s stage=%s sql=%s ctx=%v", time.Now().UTC().Format(time.RFC3339Nano), stage, query, ctx.Err())
				var err error
				switch {
				case tc.prepared:
					var stmt *sql.Stmt
					stmt, err = conn.PrepareContext(ctx, query)
					if err != nil {
						t.Fatalf("first error stage=%s.prepare: %T %v", stage, err, err)
					}
					_, err = stmt.ExecContext(ctx, args...)
					t.Logf("stage=%s.execute code=%d err=%v", stage, code, err)
					closeErr := stmt.Close()
					t.Logf("stage=%s.close err=%v", stage, closeErr)
					if err == nil {
						err = closeErr
					}
				case tc.mode == probeBlock || tc.mode == probeBlockLiteral:
					_, err = conn.ExecContext(ctx, query, args...)
				default:
					err = conn.QueryRowContext(ctx, query, args...).Scan(&code)
				}
				if err != nil {
					t.Fatalf("first error stage=%s code=%d ctx=%v: %T %v", stage, code, ctx.Err(), err, err)
				}
				t.Logf("stage=%s code=%d", stage, code)
				if code != 0 {
					t.Fatalf("stage=%s returned %d (session will be destroyed)", stage, code)
				}
			}
			probeOne(ctx, t, conn, "after")
		})
	}
}

// Metadata has its own connection so it cannot poison a lock comparison.
func TestLockProbeMetadata(t *testing.T) {
	dsn := os.Getenv("MIGRATE_PLUS_DM_LOCK_PROBE_DSN")
	if dsn == "" {
		t.Skip("set MIGRATE_PLUS_DM_LOCK_PROBE_DSN for native metadata diagnostics")
	}
	conn := probeConnection(t, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rows, err := conn.QueryContext(ctx, `SELECT BANNER FROM V$VERSION`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var banner string
		if err := rows.Scan(&banner); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		t.Logf("banner=%s", banner)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var schema string
	var mode, backslash int
	if err := conn.QueryRowContext(ctx, `SELECT SF_GET_SCHEMA_NAME_BY_ID(CURRENT_SCHID()),
SF_GET_PARA_VALUE(2, 'COMPATIBLE_MODE'), SF_GET_PARA_VALUE(2, 'BACKSLASH_ESCAPE') FROM DUAL`).Scan(&schema, &mode, &backslash); err != nil {
		t.Fatal(err)
	}
	t.Logf("schema=%s COMPATIBLE_MODE=%d BACKSLASH_ESCAPE=%d client=gitee.com/chunanyong/dm@v1.8.23", schema, mode, backslash)
}

func probeConnection(t *testing.T, dsn string) *sql.Conn {
	t.Helper()
	db, err := sql.Open("dm", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(0) // Closing the reserved connection must destroy its session.
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := conn.Raw(func(raw any) error {
		native := raw.(*dmclient.DmConnection)
		t.Logf("%s session=%d server=%s ddlAutoCommit=%v", time.Now().UTC().Format(time.RFC3339Nano), native.SessionID, native.ServerVersion, native.DDLAutoCommit)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return conn
}

func probeOne(ctx context.Context, t *testing.T, conn *sql.Conn, stage string) {
	t.Helper()
	var one int
	if err := conn.QueryRowContext(ctx, "SELECT 1 FROM DUAL").Scan(&one); err != nil || one != 1 {
		t.Fatalf("first error stage=%s.select1 value=%d: %v", stage, one, err)
	}
	t.Logf("stage=%s.select1 ok", stage)
}
