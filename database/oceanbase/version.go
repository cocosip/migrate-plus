package oceanbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	ob "github.com/cocosip/go-oceanbase"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
)

// SetVersion atomically replaces migration state, preserving dirty NilVersion.
func (d *OceanBase) SetVersion(version int, dirty bool) (err error) {
	if version < database.NilVersion {
		return fmt.Errorf("oceanbase: version must be at least %d", database.NilVersion)
	}
	ctx, cancel := d.operationContext()
	defer cancel()
	if err := d.checkSession(ctx); err != nil {
		return err
	}
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		d.discard()
		return err
	}
	defer func() {
		rollbackErr := tx.Rollback()
		if errors.Is(rollbackErr, sql.ErrTxDone) {
			rollbackErr = nil
		}
		if rollbackErr != nil {
			d.discard()
		}
		err = errors.Join(err, rollbackErr)
		err = d.operationError(ctx, "migration state transaction", err)
	}()
	query := `DELETE FROM ` + d.versionTable()
	if _, err = tx.ExecContext(ctx, query); err != nil {
		return err
	}
	if version >= 0 || dirty {
		value := 0
		if dirty {
			value = 1
		}
		binds := "?, ?"
		if d.mode == ob.ModeOracle {
			binds = ":1, :2"
		}
		query = `INSERT INTO ` + d.versionTable() + ` (version, dirty) VALUES (` + binds + `)`
		if _, err = tx.ExecContext(ctx, query, version, value); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		d.discard()
	}
	return err
}

// Version returns NilVersion for empty history and refuses ambiguous or corrupt state.
func (d *OceanBase) Version() (version int, dirty bool, err error) {
	ctx, cancel := d.operationContext()
	defer cancel()
	version = database.NilVersion
	err = d.query(ctx, `SELECT version, dirty FROM `+d.versionTable(), func(rows *sql.Rows) error {
		if !rows.Next() {
			return nil
		}
		var value int
		if err := rows.Scan(&version, &value); err != nil {
			return err
		}
		if version < database.NilVersion || value < 0 || value > 1 || rows.Next() {
			return fmt.Errorf("oceanbase: corrupt or multiple migration state rows")
		}
		dirty = value == 1
		return nil
	})
	return version, dirty, err
}

func (d *OceanBase) ensureVersionTable() (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), migrate.DefaultLockTimeout)
	defer cancel()
	if err := d.lock(ctx); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Unlock()) }()
	if err := d.verifyLockSurvival(); err != nil {
		return err
	}
	query := `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ? AND table_name = ? AND table_type = 'BASE TABLE' AND (create_options IS NULL OR create_options NOT LIKE '%TEMPORARY%')`
	if d.mode == ob.ModeOracle {
		query = `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER = :1 AND TABLE_NAME = :2 AND TEMPORARY = 'N' AND DURATION IS NULL`
	}
	var count int
	if err := d.operationError(ctx, query, d.conn.QueryRowContext(ctx, query, d.schema, d.config.MigrationsTable).Scan(&count)); err != nil {
		return err
	}
	if count != 0 {
		if err := d.validateVersionColumns(ctx); err != nil {
			return err
		}
		_, _, err = d.Version()
		return err
	}
	columns := `version BIGINT NOT NULL PRIMARY KEY, dirty TINYINT NOT NULL CHECK (dirty IN (0, 1))`
	suffix := ` ENGINE=InnoDB`
	if d.mode == ob.ModeOracle {
		columns = `version NUMBER(19) NOT NULL PRIMARY KEY, dirty NUMBER(1) NOT NULL CHECK (dirty IN (0, 1))`
		suffix = ""
	}
	return d.execContext(ctx, `CREATE TABLE `+d.versionTable()+` (`+columns+`)`+suffix)
}

func (d *OceanBase) validateVersionColumns(ctx context.Context) error {
	query := `SELECT column_name, data_type, numeric_precision, COALESCE(numeric_scale, 0), is_nullable FROM information_schema.columns WHERE table_schema = ? AND table_name = ?`
	if d.mode == ob.ModeOracle {
		query = `SELECT column_name, data_type, data_precision, NVL(data_scale, 0), nullable FROM ALL_TAB_COLUMNS WHERE owner = :1 AND table_name = :2`
	}
	seen := make(map[string]bool)
	return d.query(ctx, query, func(rows *sql.Rows) error {
		for rows.Next() {
			var name, kind, nullable string
			var precision sql.NullInt64
			var scale int
			if err := rows.Scan(&name, &kind, &precision, &scale, &nullable); err != nil {
				return err
			}
			name = strings.ToLower(name)
			kind = strings.ToLower(kind)
			if name != "version" && name != "dirty" {
				return fmt.Errorf("oceanbase: unsupported additional migration state column")
			}
			valid := false
			switch name {
			case "version":
				// MySQL BIGINT UNSIGNED reports numeric_precision 20 and cannot store NilVersion.
				valid = (kind == "bigint" && (d.mode == ob.ModeOracle || precision.Valid && precision.Int64 == 19)) ||
					((kind == "number" || kind == "decimal") && (!precision.Valid || precision.Int64 >= 19))
			case "dirty":
				valid = kind == "bigint" || kind == "tinyint" || kind == "smallint" || kind == "int" || kind == "integer" || ((kind == "number" || kind == "decimal") && (!precision.Valid || precision.Int64 >= 1))
			}
			if seen[name] || !valid || scale != 0 || (nullable != "NO" && nullable != "N") {
				return fmt.Errorf("oceanbase: migration state requires non-null integral version and dirty columns")
			}
			seen[name] = true
		}
		if len(seen) != 2 {
			return fmt.Errorf("oceanbase: missing migration state columns")
		}
		return nil
	}, d.schema, d.config.MigrationsTable)
}
