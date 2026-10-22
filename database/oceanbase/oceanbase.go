package oceanbase

import (
	"context"
	"crypto/sha256"
	"database/sql"
	sqldriver "database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"

	ob "github.com/cocosip/go-oceanbase"
	"github.com/golang-migrate/migrate/v4/database"
)

func init() { database.Register("oceanbase", &OceanBase{}) }

// OceanBase reserves one physical session for metadata, execution and migration locks.
type OceanBase struct {
	db                                       *sql.DB
	conn                                     *sql.Conn
	config                                   Config
	mode                                     ob.Mode
	schema, serverVersion, sqlMode, lockName string
	lockID                                   int64
	isLocked, broken, closed                 atomic.Bool
	lockMu                                   sync.Mutex
	lockCtx                                  context.Context
	stopLock                                 context.CancelFunc
	closeOnce                                sync.Once
	closeErr                                 error
}

var _ database.Driver = (*OceanBase)(nil)

// WithInstance takes pool ownership on success. Failure leaves the pool caller-owned.
// Supply a dedicated, idle go-oceanbase pool with autocommit and AllowMultiStatements enabled.
func WithInstance(instance *sql.DB, config *Config) (_ database.Driver, err error) {
	if instance == nil {
		return nil, fmt.Errorf("oceanbase: nil database instance")
	}
	normalized, err := prepareConfig(config)
	if err != nil {
		return nil, err
	}
	d := &OceanBase{db: instance, config: normalized}
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
			_ = d.conn.Close()
		}
	}()
	if err = d.initialize(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *OceanBase) initialize(ctx context.Context) error {
	rows, err := ob.Metadata(ctx, d.conn, ob.MetadataFilter{Kind: ob.MetadataServerInfo})
	if err != nil {
		return fmt.Errorf("oceanbase: native server metadata: %w", err)
	}
	var mode string
	var parameters, types int
	if !rows.Next() {
		return errors.Join(sql.ErrNoRows, rows.Err(), rows.Close())
	}
	err = rows.Scan(&d.serverVersion, &mode, &parameters, &types)
	if err == nil && rows.Next() {
		err = fmt.Errorf("oceanbase: ambiguous server metadata")
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	if d.serverVersion == "" {
		return fmt.Errorf("oceanbase: missing OceanBase server version")
	}
	switch mode {
	case "MYSQL":
		d.mode = ob.ModeMySQL
	case "ORACLE":
		d.mode = ob.ModeOracle
		d.config.MigrationsTable = strings.ToUpper(d.config.MigrationsTable)
	default:
		return fmt.Errorf("oceanbase: unsupported server compatibility mode")
	}
	if err := d.checkSession(ctx); err != nil {
		return err
	}
	// The native client rejects BeginTx on an already active transaction; never commit caller work.
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := tx.Rollback(); err != nil {
		return err
	}
	// Native user-lock namespaces are tenant-scoped. Hash the exact schema/table within that tenant.
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", d.mode, d.schema, d.config.MigrationsTable)))
	d.lockName = fmt.Sprintf("migrate-plus:%x", digest[:20])
	d.lockID = int64(binary.BigEndian.Uint32(digest[:4]) & 0x3fffffff)
	return d.ensureVersionTable()
}

// Open parses native connection settings after removing migrate-specific options.
func (*OceanBase) Open(raw string) (database.Driver, error) {
	native, config, err := parseURL(raw)
	if err != nil {
		return nil, err
	}
	connector, err := ob.NewConnector(native)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	d, err := WithInstance(db, config)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return d, nil
}

// Close cancels lock waits, releases any held lock and closes the owned pool.
func (d *OceanBase) Close() error {
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

// Run preflights the complete file and executes native SQL without dialect translation.
func (d *OceanBase) Run(migration io.Reader) error {
	if err := d.usable(); err != nil {
		return err
	}
	script, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	ctx, cancel := d.operationContext()
	defer cancel()
	if err := d.checkSession(ctx); err != nil {
		return err
	}
	statements, err := splitStatements(string(script), d.mode, d.sqlMode)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if err := d.execContext(ctx, statement); err != nil {
			return err
		}
	}
	return d.checkSession(ctx)
}

func (d *OceanBase) checkSession(ctx context.Context) error {
	if err := d.usable(); err != nil {
		return err
	}
	var schema sql.NullString
	var autocommit, lower int
	var sqlMode string
	query := `SELECT DATABASE(), @@SESSION.autocommit, @@SESSION.sql_mode, @@lower_case_table_names`
	var err error
	if d.mode == ob.ModeOracle {
		query = `SELECT SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA'), @@SESSION.autocommit FROM dual`
		err = d.conn.QueryRowContext(ctx, query).Scan(&schema, &autocommit)
	} else {
		err = d.conn.QueryRowContext(ctx, query).Scan(&schema, &autocommit, &sqlMode, &lower)
	}
	if err := d.operationError(ctx, query, err); err != nil {
		return err
	}
	if !schema.Valid || schema.String == "" || autocommit != 1 || d.schema != "" && schema.String != d.schema {
		d.discard()
		return fmt.Errorf("oceanbase: migrations require autocommit in the original nonempty schema")
	}
	if d.mode == ob.ModeMySQL {
		if lower < 0 || lower > 2 {
			d.discard()
			return fmt.Errorf("oceanbase: unknown identifier case behavior")
		}
		if lower != 0 {
			d.config.MigrationsTable = strings.ToLower(d.config.MigrationsTable)
		}
	}
	d.schema, d.sqlMode = schema.String, sqlMode
	return nil
}

func (d *OceanBase) quoteIdentifier(value string) string {
	quote := `"`
	if d.mode == ob.ModeMySQL {
		quote = "`"
	}
	return quote + strings.ReplaceAll(value, quote, quote+quote) + quote
}

func (d *OceanBase) qualified(name string) string {
	return d.quoteIdentifier(d.schema) + "." + d.quoteIdentifier(name)
}
func (d *OceanBase) versionTable() string { return d.qualified(d.config.MigrationsTable) }

func (d *OceanBase) operationContext() (context.Context, context.CancelFunc) {
	if d.config.StatementTimeout > 0 {
		return context.WithTimeout(context.Background(), d.config.StatementTimeout)
	}
	return context.WithCancel(context.Background())
}

func (d *OceanBase) usable() error {
	if d.broken.Load() || d.closed.Load() {
		return fmt.Errorf("oceanbase: migration session is closed; construct a new driver: %w", sql.ErrConnDone)
	}
	return nil
}

func (d *OceanBase) discard() {
	if d.broken.CompareAndSwap(false, true) {
		d.stopLock()
		_ = d.conn.Raw(func(any) error { return sqldriver.ErrBadConn })
	}
}

func (d *OceanBase) operationError(ctx context.Context, query string, err error) error {
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if err == nil {
		return nil
	}
	var network net.Error
	var native *ob.Error
	fatal := errors.Is(err, sqldriver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network)
	if errors.As(err, &native) {
		fatal = fatal || strings.HasPrefix(native.SQLState, "08")
	}
	if fatal {
		d.discard()
	}
	return &database.Error{OrigErr: err, Err: "oceanbase operation failed", Query: []byte(query)}
}

func (d *OceanBase) execContext(ctx context.Context, query string, args ...any) error {
	if err := d.usable(); err != nil {
		return err
	}
	_, err := d.conn.ExecContext(ctx, query, args...)
	return d.operationError(ctx, query, err)
}

func (d *OceanBase) query(ctx context.Context, query string, scan func(*sql.Rows) error, args ...any) (err error) {
	if err := d.usable(); err != nil {
		return err
	}
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
