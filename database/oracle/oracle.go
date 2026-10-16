package oracle

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	go_ora "github.com/sijms/go-ora/v2"
)

func init() {
	database.Register("oracle", &Oracle{})
}

// Oracle holds a dedicated session so DDL cannot release the migration lock.
type Oracle struct {
	db        *sql.DB
	conn      *sql.Conn
	config    Config
	schema    string
	lockID    int64
	isLocked  atomic.Bool
	broken    atomic.Bool
	closed    atomic.Bool
	lockMu    sync.Mutex
	lockCtx   context.Context
	stopLock  context.CancelFunc
	closeOnce sync.Once
	closeErr  error
}

var _ database.Driver = (*Oracle)(nil)

// WithInstance takes ownership of instance on success; Close closes its pool.
// On failure the pool remains caller-owned and the reserved session is discarded.
func WithInstance(instance *sql.DB, config *Config) (_ database.Driver, err error) {
	if instance == nil {
		return nil, fmt.Errorf("oracle: nil database instance")
	}
	normalized, err := prepareConfig(config, capabilities{identifierLimit: 128})
	if err != nil {
		return nil, err
	}
	o := &Oracle{db: instance, config: normalized}
	ctx, cancel := o.operationContext()
	defer cancel()
	o.conn, err = instance.Conn(ctx)
	if err != nil {
		return nil, err
	}
	o.lockCtx, o.stopLock = context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			// Never return a session with a possibly unreleased lock to the pool.
			o.discard()
		}
	}()
	if err = o.initialize(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *Oracle) initialize() error {
	var server, compatible string
	if err := o.exec(`BEGIN DBMS_UTILITY.DB_VERSION(:1, :2); END;`,
		go_ora.Out{Dest: &server, Size: 64}, go_ora.Out{Dest: &compatible, Size: 64}); err != nil {
		return err
	}
	caps, err := capabilitiesFor(server, compatible)
	if err != nil {
		return err
	}
	o.config, err = prepareConfig(&o.config, caps)
	if err != nil {
		return err
	}
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') FROM dual`
	if err := o.conn.QueryRowContext(ctx, query).Scan(&o.schema); err != nil {
		return queryError(query, err)
	}
	if o.schema == "" {
		return fmt.Errorf("oracle: empty current schema")
	}
	lockID, err := database.GenerateAdvisoryLockId(o.schema, o.config.MigrationsTable)
	if err != nil {
		return err
	}
	numericID, err := strconv.ParseUint(lockID, 10, 32)
	if err != nil {
		return err
	}
	// DBMS_LOCK numeric IDs occupy 0..1073741823; hashing only risks contention.
	o.lockID = int64(numericID & 0x3fffffff)
	return o.ensureVersionTable()
}

// Open opens a go-ora URL, filtering migrate-specific x-* parameters.
func (*Oracle) Open(raw string) (database.Driver, error) {
	dsn, config, err := parseURL(raw)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, err
	}
	d, err := WithInstance(db, config)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return d, nil
}

// Close releases the migration lock and closes the session and owned pool.
func (o *Oracle) Close() error {
	o.closeOnce.Do(func() {
		o.stopLock()
		o.lockMu.Lock()
		defer o.lockMu.Unlock()
		if o.isLocked.Load() && !o.broken.Load() {
			o.closeErr = o.unlock()
			if o.closeErr != nil {
				o.discard()
			}
		}
		o.closed.Store(true)
		connErr := o.conn.Close()
		if errors.Is(connErr, sql.ErrConnDone) {
			connErr = nil
		}
		o.closeErr = errors.Join(o.closeErr, connErr, o.db.Close())
	})
	return o.closeErr
}

// Lock acquires a session lock that survives Oracle's implicit DDL commits.
func (o *Oracle) Lock() error {
	return o.lock(context.Background())
}

func (o *Oracle) lock(parent context.Context) error {
	if !o.lockMu.TryLock() {
		return database.ErrLocked
	}
	defer o.lockMu.Unlock()
	if err := o.usable(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if o.config.StatementTimeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, o.config.StatementTimeout)
		defer stop()
	}
	stop := context.AfterFunc(o.lockCtx, cancel)
	defer stop()
	return database.CasRestoreOnErr(&o.isLocked, false, true, database.ErrLocked, func() error {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			code := -1
			query := `BEGIN :1 := DBMS_LOCK.REQUEST(id => :2, lockmode => 6, timeout => 0, release_on_commit => FALSE); END;`
			if err := o.execContext(ctx, query, sql.Out{Dest: &code}, o.lockID); err != nil {
				return err
			}
			switch code {
			case 0:
				return nil
			case 1:
			case 4:
				return database.ErrLocked
			default:
				return fmt.Errorf("oracle: DBMS_LOCK.REQUEST returned %d", code)
			}
			select {
			case <-ctx.Done():
				return errors.Join(database.ErrLocked, ctx.Err())
			case <-ticker.C:
			}
		}
	})
}

// Unlock releases the session lock; a failed release can be retried.
func (o *Oracle) Unlock() error {
	o.lockMu.Lock()
	defer o.lockMu.Unlock()
	return o.unlock()
}

func (o *Oracle) unlock() error {
	return database.CasRestoreOnErr(&o.isLocked, true, false, database.ErrNotLocked, func() error {
		code := -1
		query := `BEGIN :1 := DBMS_LOCK.RELEASE(:2); END;`
		if err := o.exec(query, sql.Out{Dest: &code}, o.lockID); err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("oracle: DBMS_LOCK.RELEASE returned %d", code)
		}
		return nil
	})
}

// Run frames Oracle SQL/PLSQL statements and executes them without rewriting.
func (o *Oracle) Run(migration io.Reader) error {
	if err := o.usable(); err != nil {
		return err
	}
	script, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	statements, err := splitStatements(string(script))
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if err := o.exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// SetVersion atomically persists version and dirty state, including dirty -1.
func (o *Oracle) SetVersion(version int, dirty bool) (err error) {
	if version < database.NilVersion {
		return fmt.Errorf("oracle: version must be at least %d", database.NilVersion)
	}
	if err := o.usable(); err != nil {
		return err
	}
	ctx, cancel := o.operationContext()
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			err = errors.Join(err, ctx.Err())
			o.discard()
		}
	}()
	tx, err := o.conn.BeginTx(ctx, nil)
	if err != nil {
		o.discard()
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
			o.discard()
		}
	}()
	query := `DELETE FROM ` + o.versionTable()
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return queryError(query, err)
	}
	if version >= 0 || dirty {
		dirtyValue := 0
		if dirty {
			dirtyValue = 1
		}
		query = `INSERT INTO ` + o.versionTable() + ` (version, dirty) VALUES (:1, :2)`
		if _, err := tx.ExecContext(ctx, query, version, dirtyValue); err != nil {
			return queryError(query, err)
		}
	}
	if err := tx.Commit(); err != nil {
		o.discard()
		return err
	}
	return nil
}

// Version returns NilVersion for empty state and rejects ambiguous/corrupt history.
func (o *Oracle) Version() (version int, dirty bool, err error) {
	if err := o.usable(); err != nil {
		return 0, false, err
	}
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT version, dirty FROM ` + o.versionTable()
	rows, err := o.conn.QueryContext(ctx, query)
	if err != nil {
		return 0, false, queryError(query, err)
	}
	defer func() {
		if err = errors.Join(err, rows.Close()); err != nil {
			err = queryError(query, err)
		}
	}()
	if !rows.Next() {
		return database.NilVersion, false, rows.Err()
	}
	var dirtyValue int
	if err := rows.Scan(&version, &dirtyValue); err != nil {
		return 0, false, err
	}
	if version < database.NilVersion || (dirtyValue != 0 && dirtyValue != 1) || rows.Next() {
		return 0, false, fmt.Errorf("oracle: corrupt or multiple migration state rows")
	}
	return version, dirtyValue == 1, rows.Err()
}

func (o *Oracle) ensureVersionTable() (err error) {
	lockCtx, cancelLock := context.WithTimeout(context.Background(), migrate.DefaultLockTimeout)
	defer cancelLock()
	if err := o.lock(lockCtx); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, o.Unlock()) }()
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER = :1 AND TABLE_NAME = :2`
	var count int
	if err := o.conn.QueryRowContext(ctx, query, o.schema, o.config.MigrationsTable).Scan(&count); err != nil {
		return queryError(query, err)
	}
	if count > 0 {
		_, _, err = o.Version()
		return err
	}
	return o.exec(`CREATE TABLE ` + o.versionTable() + ` (version NUMBER(19) NOT NULL PRIMARY KEY, dirty NUMBER(1) NOT NULL CHECK (dirty IN (0, 1)))`)
}

func (o *Oracle) versionTable() string {
	return quoteIdentifier(o.schema) + "." + quoteIdentifier(o.config.MigrationsTable)
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func (o *Oracle) operationContext() (context.Context, context.CancelFunc) {
	if o.config.StatementTimeout > 0 {
		return context.WithTimeout(context.Background(), o.config.StatementTimeout)
	}
	return context.WithCancel(context.Background())
}

func (o *Oracle) exec(query string, args ...any) error {
	ctx, cancel := o.operationContext()
	defer cancel()
	return o.execContext(ctx, query, args...)
}

func (o *Oracle) execContext(ctx context.Context, query string, args ...any) error {
	if err := o.usable(); err != nil {
		return err
	}
	_, err := o.conn.ExecContext(ctx, query, args...)
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
		o.discard()
	}
	if err != nil {
		return queryError(query, err)
	}
	return nil
}

func (o *Oracle) usable() error {
	if o.broken.Load() || o.closed.Load() {
		return fmt.Errorf("oracle: migration session is closed; construct a new driver: %w", sql.ErrConnDone)
	}
	return nil
}

func (o *Oracle) discard() {
	if o.broken.CompareAndSwap(false, true) {
		o.stopLock()
		_ = o.conn.Raw(func(any) error { return sqldriver.ErrBadConn })
	}
}

func queryError(query string, err error) error {
	return &database.Error{OrigErr: err, Query: []byte(query)}
}
