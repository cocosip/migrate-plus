package oceanbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	ob "github.com/cocosip/go-oceanbase"
	"github.com/golang-migrate/migrate/v4/database"
)

// Lock acquires a native tenant-scoped session lock, polling without replaying SQL.
func (d *OceanBase) Lock() error { return d.lock(context.Background()) }

func (d *OceanBase) lock(parent context.Context) error {
	if !d.lockMu.TryLock() {
		return database.ErrLocked
	}
	defer d.lockMu.Unlock()
	if err := d.usable(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if d.config.StatementTimeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, d.config.StatementTimeout)
		defer stop()
	}
	stop := context.AfterFunc(d.lockCtx, cancel)
	defer stop()
	return database.CasRestoreOnErr(&d.isLocked, false, true, database.ErrLocked, func() error {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			if ctx.Err() != nil {
				return errors.Join(database.ErrLocked, ctx.Err())
			}
			code, err := d.requestLock(ctx)
			if err != nil {
				d.discard()
				return err
			}
			if d.mode == ob.ModeMySQL && code == 1 || d.mode == ob.ModeOracle && code == 0 {
				return nil
			}
			if d.mode == ob.ModeMySQL && code != 0 || d.mode == ob.ModeOracle && code != 1 {
				d.discard()
				return fmt.Errorf("oceanbase: unexpected lock result %d", code)
			}
			select {
			case <-ctx.Done():
				return errors.Join(database.ErrLocked, ctx.Err())
			case <-ticker.C:
			}
		}
	})
}

func (d *OceanBase) requestLock(ctx context.Context) (int, error) {
	code := -1
	if d.mode == ob.ModeOracle {
		query := `BEGIN :1 := DBMS_LOCK.REQUEST(:2, 6, 0, FALSE); END;`
		err := d.execContext(ctx, query, sql.Out{Dest: &code}, d.lockID)
		return code, err
	}
	query := `SELECT GET_LOCK(?, 0)`
	err := d.conn.QueryRowContext(ctx, query, d.lockName).Scan(&code)
	return code, d.operationError(ctx, query, err)
}

// Unlock only reports success when the server confirms release.
func (d *OceanBase) Unlock() error { d.lockMu.Lock(); defer d.lockMu.Unlock(); return d.unlock() }

func (d *OceanBase) unlock() error {
	return database.CasRestoreOnErr(&d.isLocked, true, false, database.ErrNotLocked, func() error {
		if err := d.usable(); err != nil {
			return err
		}
		ctx, cancel := d.operationContext()
		defer cancel()
		code := -1
		var err error
		if d.mode == ob.ModeOracle {
			err = d.execContext(ctx, `BEGIN :1 := DBMS_LOCK.RELEASE(:2); END;`, sql.Out{Dest: &code}, d.lockID)
		} else {
			query := `SELECT RELEASE_LOCK(?)`
			err = d.operationError(ctx, query, d.conn.QueryRowContext(ctx, query, d.lockName).Scan(&code))
		}
		if err != nil {
			return err
		}
		expected := 0
		if d.mode == ob.ModeMySQL {
			expected = 1
		}
		if code != expected {
			d.discard()
			return fmt.Errorf("oceanbase: unexpected unlock result %d", code)
		}
		return nil
	})
}

// verifyLockSurvival rejects transaction-scoped DBMS_LOCK implementations before state DDL.
func (d *OceanBase) verifyLockSurvival() error {
	ctx, cancel := d.operationContext()
	defer cancel()
	for _, command := range []string{keywordCommit, keywordRollback} {
		if err := d.execContext(ctx, command); err != nil {
			return err
		}
		held := false
		if d.mode == ob.ModeOracle {
			code, err := d.requestLock(ctx)
			if err != nil {
				return err
			}
			held = code == 4
		} else {
			query := `SELECT IS_USED_LOCK(?) = CONNECTION_ID()`
			var value sql.NullInt64
			if err := d.operationError(ctx, query, d.conn.QueryRowContext(ctx, query, d.lockName).Scan(&value)); err != nil {
				return err
			}
			held = value.Valid && value.Int64 == 1
		}
		if !held {
			d.discard()
			return fmt.Errorf("oceanbase: native lock does not survive %s; a session lock is required for DDL migrations", command)
		}
	}
	return nil
}
