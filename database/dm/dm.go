package dm

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dmclient "gitee.com/chunanyong/dm"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
)

func init() { database.Register("dm", &Dm{}) }

// Dm reserves one DM session for metadata, SQL execution and the migration lock.
type Dm struct {
	db              *sql.DB
	conn            *sql.Conn
	config          Config
	caps            capabilities
	schema          string
	schemaID        int64
	lockID          int64
	protocolVersion string
	ddlAutoCommit   bool
	isLocked        atomic.Bool
	broken          atomic.Bool
	closed          atomic.Bool
	closeOnce       sync.Once
	closeErr        error
	lockMu          sync.Mutex
	lockCtx         context.Context
	stopLock        context.CancelFunc
}

var _ database.Driver = (*Dm)(nil)

// WithInstance takes pool ownership on success; Close closes both session and pool.
// The caller must use a dedicated native DM pool with autoCommit=true,
// rwSeparate=0, doSwitch=0 and driverReconnect=false. Failure leaves the pool open.
// Queries must use maxRows=0, enRsCache=false and escapeProcess=false.
func WithInstance(instance *sql.DB, config *Config) (_ database.Driver, err error) {
	if instance == nil {
		return nil, fmt.Errorf("dm: nil database instance")
	}
	normalized, err := prepareConfig(config)
	if err != nil {
		return nil, err
	}
	d := &Dm{db: instance, config: normalized}
	ctx, cancel := d.operationContext()
	defer cancel()
	d.conn, err = instance.Conn(ctx)
	if err != nil {
		return nil, err
	}
	d.lockCtx, d.stopLock = context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			d.discard()
		}
	}()
	if err := d.initialize(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Dm) initialize() error {
	var banners []string
	if err := d.query(`SELECT BANNER FROM V$VERSION`, func(rows *sql.Rows) error {
		for rows.Next() {
			var banner string
			if err := rows.Scan(&banner); err != nil {
				return err
			}
			banners = append(banners, banner)
		}
		return nil
	}); err != nil {
		return err
	}
	var mode, caseSensitive, backslash int
	// BACKSLASH_ESCAPE is static; read the active memory value, not a session parameter or the INI file.
	query := `SELECT CURRENT_SCHID(), SF_GET_SCHEMA_NAME_BY_ID(CURRENT_SCHID()),
SF_GET_PARA_VALUE(2, 'COMPATIBLE_MODE'), SF_GET_CASE_SENSITIVE_FLAG(),
SF_GET_PARA_VALUE(2, 'BACKSLASH_ESCAPE') FROM DUAL`
	if err := d.query(query, func(rows *sql.Rows) error {
		if !rows.Next() {
			return sql.ErrNoRows
		}
		return rows.Scan(&d.schemaID, &d.schema, &mode, &caseSensitive, &backslash)
	}); err != nil {
		return err
	}
	if d.schemaID <= 0 || d.schema == "" {
		return fmt.Errorf("dm: invalid current schema identity")
	}
	var err error
	d.caps, err = capabilitiesFor(banners, mode, caseSensitive, backslash)
	if err != nil {
		return err
	}
	if err := d.conn.Raw(func(raw any) error {
		if native, ok := raw.(*dmclient.DmConnection); ok {
			d.protocolVersion, d.ddlAutoCommit = native.ServerVersion, native.DDLAutoCommit
			if native.BackSlashFlag != d.caps.backslash {
				return fmt.Errorf("dm: native client and session disagree on BACKSLASH_ESCAPE; reopen with consistent settings")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	lockID, err := database.GenerateAdvisoryLockId(strconv.FormatInt(d.schemaID, 10), d.config.MigrationsTable)
	if err != nil {
		return err
	}
	numericID, err := strconv.ParseUint(lockID, 10, 32)
	if err != nil {
		return err
	}
	d.lockID = int64(numericID & 0x3fffffff)
	return d.ensureVersionTable()
}

// Open opens a native DM DSN after filtering migration options and fixing session invariants.
func (*Dm) Open(raw string) (database.Driver, error) {
	dsn, config, err := parseURL(raw)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("dm", dsn)
	if err != nil {
		return nil, err
	}
	d, err := WithInstance(db, config)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return d, nil
}

// Close releases the lock and closes the owned pool. A failed release kills the session.
func (d *Dm) Close() error {
	d.closeOnce.Do(func() {
		d.stopLock()
		d.lockMu.Lock()
		defer d.lockMu.Unlock()
		if d.isLocked.Load() && !d.broken.Load() {
			d.closeErr = d.unlock()
			if d.closeErr != nil {
				d.discard()
			}
		}
		d.closed.Store(true)
		connErr := d.conn.Close()
		if errors.Is(connErr, sql.ErrConnDone) {
			connErr = nil
		}
		d.closeErr = errors.Join(d.closeErr, connErr, d.db.Close())
	})
	return d.closeErr
}

// Lock takes an exclusive session lock that survives commit, rollback and DDL.
func (d *Dm) Lock() error {
	return d.lock(context.Background())
}

func (d *Dm) lock(parent context.Context) error {
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
			code := -1
			// Mixed OUT/IN binds in an anonymous block break some DM8 sessions.
			query := `SELECT DBMS_LOCK.REQUEST(?, 6, 0, FALSE) FROM DUAL`
			err := d.conn.QueryRowContext(ctx, query, d.lockID).Scan(&code)
			if err := d.operationError(ctx, query, err); err != nil {
				return err
			}
			if code == 0 {
				return nil
			}
			if code != 1 {
				return fmt.Errorf("dm: DBMS_LOCK.REQUEST returned %d", code)
			}
			select {
			case <-ctx.Done():
				return errors.Join(database.ErrLocked, ctx.Err())
			case <-ticker.C:
			}
		}
	})
}

// Unlock releases the native lock; a failed release remains retryable on a live session.
func (d *Dm) Unlock() error {
	d.lockMu.Lock()
	defer d.lockMu.Unlock()
	return d.unlock()
}

func (d *Dm) unlock() error {
	return database.CasRestoreOnErr(&d.isLocked, true, false, database.ErrNotLocked, func() error {
		if err := d.usable(); err != nil {
			return err
		}
		ctx, cancel := d.operationContext()
		defer cancel()
		code := -1
		query := `SELECT DBMS_LOCK.RELEASE(?) FROM DUAL`
		err := d.conn.QueryRowContext(ctx, query, d.lockID).Scan(&code)
		if err := d.operationError(ctx, query, err); err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("dm: DBMS_LOCK.RELEASE returned %d", code)
		}
		return nil
	})
}

// Run preflights statement boundaries, then executes DM SQL without dialect rewriting.
func (d *Dm) Run(migration io.Reader) error {
	if err := d.usable(); err != nil {
		return err
	}
	script, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	statements, err := splitStatements(string(script), d.caps)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if err := d.exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// SetVersion persists upstream's version/dirty state with transactional DELETE/INSERT.
func (d *Dm) SetVersion(version int, dirty bool) (err error) {
	if version < database.NilVersion {
		return fmt.Errorf("dm: version must be at least %d", database.NilVersion)
	}
	if err := d.usable(); err != nil {
		return err
	}
	ctx, cancel := d.operationContext()
	defer cancel()
	query := `DELETE FROM ` + d.versionTable()
	defer func() { err = d.operationError(ctx, query, err) }()
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		d.discard()
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
			d.discard()
		}
	}()
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return err
	}
	if version >= 0 || dirty {
		value := 0
		if dirty {
			value = 1
		}
		query = `INSERT INTO ` + d.versionTable() + ` ("VERSION", "DIRTY") VALUES (?, ?)`
		if _, err := tx.ExecContext(ctx, query, version, value); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		d.discard()
		return err
	}
	return nil
}

// Version returns NilVersion for empty state and rejects ambiguous/corrupt history.
func (d *Dm) Version() (version int, dirty bool, err error) {
	version = database.NilVersion
	err = d.query(`SELECT "VERSION", "DIRTY" FROM `+d.versionTable(), func(rows *sql.Rows) error {
		if !rows.Next() {
			return nil
		}
		var dirtyValue any
		if err := rows.Scan(&version, &dirtyValue); err != nil {
			return err
		}
		if version < database.NilVersion || rows.Next() {
			return fmt.Errorf("dm: corrupt or multiple migration state rows")
		}
		// The native client returns BIT/integer widths directly and integral DECIMAL as text.
		switch dirtyValue {
		case false, int8(0), int16(0), int32(0), int64(0), "0":
			dirty = false
		case true, int8(1), int16(1), int32(1), int64(1), "1":
			dirty = true
		default:
			return fmt.Errorf("dm: invalid DIRTY value %v (%T); expected 0/false or 1/true", dirtyValue, dirtyValue)
		}
		return nil
	})
	return version, dirty, err
}

func (d *Dm) ensureVersionTable() (err error) {
	lockCtx, cancelLock := context.WithTimeout(context.Background(), migrate.DefaultLockTimeout)
	defer cancelLock()
	if err := d.lock(lockCtx); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, d.Unlock()) }()
	name := "NAME"
	if !d.caps.caseSensitive {
		name = "UPPER(NAME)"
	}
	count := 0
	query := `SELECT COUNT(*) FROM SYS.SYSOBJECTS WHERE SCHID = ? AND TYPE$ = 'SCHOBJ' AND SUBTYPE$ = 'UTAB' AND BITAND(INFO3, 255) = 0 AND BITAND(INFO3, 131072) = 0 AND ` + name + ` = ?`
	if err := d.query(query, func(rows *sql.Rows) error {
		if !rows.Next() {
			return sql.ErrNoRows
		}
		return rows.Scan(&count)
	}, d.schemaID, d.config.MigrationsTable); err != nil {
		return err
	}
	if count == 0 {
		if err := d.exec(`CREATE TABLE ` + d.versionTable() + ` ("VERSION" BIGINT NOT NULL PRIMARY KEY, "DIRTY" SMALLINT NOT NULL CHECK ("DIRTY" IN (0, 1)))`); err != nil {
			return err
		}
	} else if count != 1 {
		return fmt.Errorf("dm: ambiguous migration table identity")
	}
	if err := d.query(`SELECT "VERSION", "DIRTY" FROM `+d.versionTable()+` WHERE 1 = 0`, validateVersionColumns); err != nil {
		return err
	}
	_, _, err = d.Version()
	return err
}

func validateVersionColumns(rows *sql.Rows) error {
	columns, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	if len(columns) != 2 {
		return fmt.Errorf("dm: migration table must provide VERSION and DIRTY")
	}
	for i, column := range columns {
		precision, scale, sized := column.DecimalSize()
		switch strings.ToUpper(column.DatabaseTypeName()) {
		case "BIGINT":
		case "SMALLINT", "INTEGER", "INT", "TINYINT", "BIT":
			if i == 0 {
				return fmt.Errorf("dm: VERSION requires a 64-bit integer column")
			}
		case "NUMBER", "NUMERIC", "DECIMAL", "DEC":
			if !sized || scale != 0 || (i == 0 && precision < 19) {
				return fmt.Errorf("dm: migration state columns require sufficient integral precision")
			}
		default:
			return fmt.Errorf("dm: migration state columns must be numeric integers")
		}
	}
	return nil
}

func (d *Dm) versionTable() string {
	return quoteIdentifier(d.schema) + "." + quoteIdentifier(d.config.MigrationsTable)
}

func quoteIdentifier(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func (d *Dm) operationContext() (context.Context, context.CancelFunc) {
	if d.config.StatementTimeout > 0 {
		return context.WithTimeout(context.Background(), d.config.StatementTimeout)
	}
	return context.WithCancel(context.Background())
}

func (d *Dm) usable() error {
	if d.broken.Load() || d.closed.Load() {
		return fmt.Errorf("dm: migration session is closed; construct a new driver: %w", sql.ErrConnDone)
	}
	return nil
}

func (d *Dm) discard() {
	if d.broken.CompareAndSwap(false, true) {
		d.stopLock()
		_ = d.conn.Raw(func(any) error { return sqldriver.ErrBadConn })
	}
}

func (d *Dm) operationError(ctx context.Context, query string, err error) error {
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if err == nil {
		return nil
	}
	var native *dmclient.DmError
	var network net.Error
	fatal := errors.Is(err, sqldriver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network)
	if errors.As(err, &native) {
		switch native.ErrCode {
		case 6001, 6002, 6060, 20000, 20001, 20004:
			fatal = true
		}
	}
	if fatal {
		d.discard()
	}
	return database.Error{OrigErr: err, Err: "dm operation failed", Query: []byte(query)}
}

func (d *Dm) exec(query string) error {
	ctx, cancel := d.operationContext()
	defer cancel()
	if err := d.usable(); err != nil {
		return err
	}
	_, err := d.conn.ExecContext(ctx, query)
	return d.operationError(ctx, query, err)
}

func (d *Dm) query(query string, scan func(*sql.Rows) error, args ...any) (err error) {
	if err := d.usable(); err != nil {
		return err
	}
	ctx, cancel := d.operationContext()
	defer cancel()
	defer func() { err = d.operationError(ctx, query, err) }()
	rows, err := d.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	if err := scan(rows); err != nil {
		return err
	}
	return rows.Err()
}
