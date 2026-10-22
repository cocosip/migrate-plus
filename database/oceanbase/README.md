# OceanBase migration driver

**Status: implementation checkpoint, not release-ready.** The library foundation
and offline tests are implemented. The distributed CLI does not import this
driver yet. Native protocol tests and disposable-schema integration tests are
next-phase work; neither compatibility mode has real-server validation here.
See [development handoff](../../docs/OCEANBASE_PROGRESS.md).

The package implements upstream `database.Driver` using
`github.com/cocosip/go-oceanbase v0.1.1`. Consumers keep using
`github.com/golang-migrate/migrate/v4` and import this package to register
`oceanbase`. Existing Oracle, DM, GaussDB and openGauss drivers are unchanged.

## Connection configuration

Native URL examples (placeholders, not verified deployment examples):

```text
oceanbase://app%40tenant:password@127.0.0.1:2881/demo?mode=mysql
oceanbase://app%40tenant:password@127.0.0.1:2881/?mode=oracle
```

`mode=auto` uses the native client's authenticated mode detection. Explicit
mode mismatches are rejected by the native client. Credentials use URL encoding.
The URL opener removes these migration options before native DSN parsing:

| Option | Meaning |
|---|---|
| `x-migrations-table` | Default `schema_migrations`; a plain ASCII identifier, at most 64 bytes |
| `x-statement-timeout` | Positive milliseconds; omitted means no operation deadline |

Duplicate, empty and unknown migration options fail. Native unknown options
also fail. The opener enables `allowMultiStatements=true`; explicitly disabling
it is rejected. Database auto-creation and explicitly read-only routing are
rejected. Configure native TLS, credentials and transport policies as appropriate
through the native connector; this package does not add TLS defaults.

`WithInstance(*sql.DB, *Config)` requires a dedicated idle go-oceanbase pool,
autocommit enabled, and `AllowMultiStatements=true`. It reserves a physical
connection, verifies native metadata and checks that a transaction can begin and
roll back without committing caller work. Success transfers pool ownership to
the migration driver. Failure discards the reserved session and leaves the pool
caller-owned. `Close` releases the lock and closes both session and owned pool.
Do not pass a shared application pool.

## Lock prerequisites

MySQL uses `GET_LOCK`, `IS_USED_LOCK` and `RELEASE_LOCK`. Oracle uses
`DBMS_LOCK.REQUEST` with exclusive mode and `release_on_commit => FALSE`, and
`DBMS_LOCK.RELEASE`. Oracle requires the corresponding package execution grant.
Locks are scoped by the native tenant namespace and a hash of mode, actual
schema and migrations table. The Oracle numeric ID occupies 30 bits; a collision
can cause unnecessary contention, not simultaneous migration.

Initialization checks ownership after both `COMMIT` and `ROLLBACK` before
creating or accepting the version table. Oracle expects `REQUEST` to report
already-owned (`4`); MySQL checks the owning connection ID without reacquiring
the lock. Unsupported locks, missing privileges, wrong return values and lost
ownership fail initialization. There is no process-lock or transaction-lock
fallback. Acquisition errors with uncertain ownership discard the session.

**Oracle compatibility is conditional.** Official OceanBase V4.3.5 REQUEST
documentation says `release_on_commit` can only be `TRUE`, while its example
passes `FALSE`. Servers that implement only transaction-scoped locks are
deliberately rejected. This checkpoint has no live evidence resolving that
version-specific limitation. MySQL native locks also depend on server and
OBProxy capabilities. See the linked evidence in the design document.

Initialization lock waits use `migrate.DefaultLockTimeout`; a configured statement
timeout can impose an earlier bound. Existing CLI `-lock-timeout` will apply once
the new driver is composed into that CLI. Runtime lock waits are cancelable on
driver close and bounded by `StatementTimeout` when configured.

## SQL and version state

MySQL preserves the entire script as one native multi-statement execution.
Oracle frames ordinary SQL and PL/SQL blocks; use standalone `/` lines between
procedural units. String literals, alternative Oracle quotes, quoted identifiers
and comments keep embedded semicolons opaque. MySQL preflight observes
`NO_BACKSLASH_ESCAPES` and `ANSI_QUOTES`.

SQL is executed in its original dialect. Preflight rejects visible session and
transaction commands, all `SET` statements, client commands such as `DELIMITER`
and `SOURCE`, executable comments, inline Oracle WITH routines, and malformed literals/comments. Consequently
some routine bodies using `SET` are outside this checkpoint's script subset.
Do not embed schema/identity changes, transaction control or lock operations in
procedures called by migrations. Preflight is not an SQL sandbox and does not
interpret dynamic SQL or routine side effects. The driver checks schema and
autocommit before/after `Run` and before writing state, and discards changed
sessions. Fully qualified application DDL remains the script author's responsibility.

The actual Oracle schema and version table use Oracle identifier rules; the
MySQL table respects the server's `lower_case_table_names` behavior. New state
tables use `BIGINT`/`TINYINT` (MySQL, InnoDB) or `NUMBER(19)`/`NUMBER(1)` (Oracle).
Existing tables must have exactly two non-null integral `version` and `dirty`
columns. Existing state is not rebuilt, replayed or automatically cleared.
Empty history returns upstream `NilVersion`; multiple rows, versions below `-1`
and dirty values outside `0/1` fail. Native bind/DDL compatibility and legacy
column encodings still need real-server coverage.

`SetVersion` uses transactional `DELETE`/`INSERT` and handles dirty `-1`.
Rollback errors, uncertain commits, transport failures and operation timeouts
invalidate the reserved session; it is never transparently replaced to continue
a migration. Ordinary SQL errors preserve the live session and upstream dirty
recovery behavior. Errors use upstream `database.Error`; inspect `OrigErr` for
native error code and SQLSTATE (upstream's type has no `Unwrap` method).
DDL can commit before a later statement fails. Inspect the actual schema and
history before using upstream `Force` to recover.

## Drop scope

`Drop` removes supported objects in the originally selected schema/database;
it does not drop the user, database or tenant. It preflights visible external
dependencies, object kinds and Oracle dependency order before any destructive SQL.
Only local foreign keys are removed. Catalog errors are returned rather than
treated as empty results. No forced cascade or foreign-key-check disabling is used.

Supported object kinds are tables, views, procedures, functions and triggers,
plus Oracle sequences, synonyms, packages and types. Oracle materialized views,
MySQL events and other unsupported catalog objects cause a preflight error;
remove them explicitly first. Table-owned indexes and corresponding Oracle
package/type bodies are handled by their owning object's DDL. External-dependency
detection is limited by dictionary visibility; MySQL checks visible foreign keys
and view references, not arbitrary routine-body SQL. Native catalogs, privileges,
case sensitivity and dependency visibility remain unverified against real servers.

## Validation boundary

Default tests use sqlmock and pure framing/configuration tests. They do not
establish native wire compatibility or real OceanBase behavior. No OceanBase
integration test entrypoint exists at this checkpoint. The next stage must add
separate explicit disposable MySQL/Oracle test targets, upstream conformance,
cross-session lock survival across DDL, dirty recovery, existing-history and Drop
tests. Never run destructive tests against production `GOIMAGE`.
