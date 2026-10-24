# Oracle Driver Design

Status: implemented. No Oracle server version has been verified yet.
This document extends [the project architecture](DESIGN.md). Connection examples,
account setup and test commands belong to the [Oracle driver guide](../database/oracle/README.md).

## 1. Scope and decisions

`database/oracle` implements the upstream
`github.com/golang-migrate/migrate/v4/database.Driver` and registers `oracle`.
The distributed `cmd/migrate-plus` imports the driver. Applications keep using
upstream migrate constructors, sources, version transitions and recovery commands.

Use one pure-Go Oracle driver with capability checks where Oracle versions
actually differ. User-authored numbered SQL targets the actual server version;
the driver does not translate dialects, generate business DDL, or replace the
upstream migration engine.

## 2. Dependency and version policy

Use `github.com/sijms/go-ora/v2 v2.9.0`, without cgo or an Oracle client
installation. The target baseline follows go-ora's documented Oracle 10.2+
range. Target compatibility is distinct from real-server certification.

Keep portable SQL shared. Add version branches only for documented capability
differences, with regression fixtures, rather than a separate implementation
for every Oracle release.

## 3. Server configuration and capabilities

Read the actual server version and `COMPATIBLE` once through
`DBMS_UTILITY.DB_VERSION`. Both must be at least 10.2. This avoids requiring
privileged `V$` version views. Resolve the current schema through
`SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA')` and retain that original namespace.

| Capability | Selection |
|---|---|
| Migration table identifier limit | 30 bytes; 128 only when both versions are at least 12.2 |
| Version state | `NUMBER(19)` version and numeric `NUMBER(1)` dirty flag; an existing table must be permanent (`TEMPORARY='N'`, no `DURATION`) with `NUMBER` columns of at least that precision and scale 0 |
| Migration lock | Exclusive `DBMS_LOCK` session lock across supported versions |
| User SQL | Executed without dialect rewriting |

Schema and normalized migration table names determine lock identity. Metadata
operations remain bound to the original schema throughout the driver lifetime.

## 4. Public API, URL and ownership

Expose `Oracle` implementing upstream `database.Driver`, and:

```go
type Config struct {
    MigrationsTable string
    StatementTimeout time.Duration
}

func WithInstance(instance *sql.DB, config *Config) (database.Driver, error)
```

A nil config selects defaults. Configuration is copied, not modified. Accept
Oracle connection URLs with the following migration options:

| Option | Contract |
|---|---|
| `x-migrations-table` | Default `schema_migrations`; uppercase ASCII identifier beginning with a letter, followed by letters, digits, `_`, `$` or `#`; version-dependent length limit |
| `x-statement-timeout` | Positive integer milliseconds per database operation; absent means no timeout |

Reject unknown, duplicate or malformed migration options and quoted or
schema-qualified table overrides. Filter migration options before opening the
native client; retain native go-ora connection settings. Credentials follow
go-ora URL encoding rules. Never include raw credential-bearing URLs in driver
validation errors.

Reserve one `*sql.Conn` for the driver lifetime. Successful `WithInstance`
transfers pool ownership: `Close` closes the session and entire pool. Failed
construction leaves the pool with the caller and discards the reserved physical
session so a possible lock cannot return to the pool. `Open` closes its own pool
if construction fails. Callers must supply a dedicated migration pool rather
than a shared GORM/application pool needed afterward.

## 5. Locking and failure lifecycle

Use `DBMS_LOCK.REQUEST` in exclusive mode, with zero wait and
`release_on_commit => FALSE`. The session lock survives DDL commits. The driver
retries contention every 100 ms; its per-operation timeout bounds the whole
attempt. Upstream `Migrate.LockTimeout` bounds the caller's wait without retrying
or cancelling `Driver.Lock`. Call `Close` after a lock timeout; it cancels and
waits for pending acquisition before releasing the session. Initialization
uses `migrate.DefaultLockTimeout` (15 seconds), set by the CLI from `-lock-timeout`
before construction, with a shorter statement timeout taking precedence.
Library callers configure this default before construction. Local lock state uses upstream lock sentinels and
`CasRestoreOnErr`; repeated `Lock` returns `ErrLocked`, and a failed release
remains retryable on a live session.

Derive the numeric ID using upstream `GenerateAdvisoryLockId` from the original
schema and normalized table name, masked to Oracle's `0..1073741823` range.
Hash collisions may cause unnecessary exclusion. Lock discovery/creation of
the version table as well as migration execution.

The account requires execution access to `SYS.DBMS_LOCK` and `DBMS_UTILITY`,
plus privileges and quota required by its scripts. The driver does not install
packages or grant privileges. `Close` attempts unlock and pool/session cleanup,
preserving cleanup errors. Migration SQL is not silently replayed after failure.

## 6. Version state and migration execution

Upstream drives `SetVersion(version, true)`, `Run`, then
`SetVersion(version, false)`. Persist changes with transactional `DELETE` and
optional `INSERT`; preserve dirty `NilVersion` and empty clean state. Never use
`TRUNCATE` for state changes because Oracle DDL commits implicitly.
Read all state rows and reject multiple rows, versions below -1, or dirty
values other than 0/1. Validate existing history under the initialization lock
as well as when upstream reads Version; never hide dirty rows with ROWNUM.
Failed begin/commit/rollback operations discard the reserved session rather
than risk continuing with uncertain transaction state. Execution timeouts also
discard the session. Recovery requires a new driver and inspection of persisted
state; no migration SQL is replayed.

Run framed statements sequentially on the reserved session and stop at the
first failure. Failed migrations retain upstream dirty state; earlier DDL may
already have committed and cannot be rolled back automatically. Recovery uses
upstream commands after the operator repairs or verifies the schema.

Configured timeouts use per-operation contexts. The upstream driver interface
does not expose end-to-end caller cancellation. SQL errors retain native error
details through upstream `database.Error`.

## 7. Statement framing

Split ordinary SQL at semicolons outside comments, quoted identifiers, strings
and Oracle q-quotes. Remove ordinary SQL's final semicolon before execution.
PL/SQL anonymous blocks and stored procedures, functions, packages, triggers
and types retain internal and final semicolons. Separate units with `/` on its
own line; the final unit may end at EOF.

Inline `WITH FUNCTION` / `WITH PROCEDURE` SQL also uses slash framing. Preserve
routine semicolons and remove the final SQL semicolon. Frame the complete input
before executing statements so lexical errors do not cause partial execution.

This is driver-local framing. SQL validation belongs to Oracle; SQL*Plus
commands, substitution variables and client scripts are not interpreted.

## 8. Drop boundary

Remove supported objects in the original schema: user tables and dependent
indexes/constraints/triggers, standalone triggers such as schema event triggers,
views, materialized views, sequences, synonyms,
packages, procedures, functions and types. Standalone triggers are dropped
first and system-generated objects are excluded by the `generated` filter.
Drop materialized views before
listing remaining objects so tables retained by `ON PREBUILT TABLE` are included.

Preflight visible external foreign keys and object dependencies before DDL.
Remove only local foreign keys explicitly, use table `PURGE` without
`CASCADE CONSTRAINTS`, and drop types in dependency order without `FORCE`.
Reject type cycles before DDL. Native dependency checks remain necessary when
catalog privileges hide external constraints. Quote catalog identifiers and
qualify them with the original schema.

Do not drop the Oracle user, database, grants, scheduler jobs or database links.
Errors stop processing; earlier DDL may already have committed. Close and reopen
after Drop. Dependencies and destructive behavior require real-server testing.

## 9. Delivery and verification

Unit tests cover version capabilities, URL and identifier validation, framing,
lock contention, connection ownership and initialization cleanup, state rollback
and execution failures. Opt-in integration tests reuse upstream
`database/testing.Test`, independent sessions, DDL lock survival and upstream
dirty/force/up/down behavior.

Integration tests run only behind the `integration` build tag
(`go test -tags integration`), require `MIGRATE_PLUS_ORACLE_TEST_URL`, and
destroy supported
objects in the selected disposable schema. See the
[test instructions](../database/oracle/README.md#integration-tests). Record actual
server versions, `COMPATIBLE` settings and results before claiming verification.
Mock-based tests and cross-builds do not prove Oracle SQL or wire compatibility.

Build, unit tests, lint, race tests and pure-Go target builds follow the shared
[project validation policy](DESIGN.md#9-testing-and-ci).

## 10. Sources and verification boundary

- [go-ora v2.9.0](https://github.com/sijms/go-ora/tree/v2.9.0)
- [Upstream database extension interface](https://github.com/golang-migrate/migrate/blob/v4.20.1/database/driver.go)
- [Oracle implementation](../database/oracle/)
- [Oracle usage and operational requirements](../database/oracle/README.md)

No Oracle server version is marked verified without real-server results. The
implemented capability policy is a compatibility target, not a certification of
every release or deployment topology.
