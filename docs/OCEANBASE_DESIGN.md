# OceanBase Driver Design

Status: implemented and included in the distributed CLI. Unit tests use sqlmock;
opt-in integration tests exist but have not been run, so no OceanBase server
version, proxy path or compatibility mode is certified.
This document extends [the project architecture](DESIGN.md). Connection examples,
account setup and test commands belong to the [OceanBase driver guide](../database/oceanbase/README.md).

## 1. Scope and decisions

Add `database/oceanbase` as an upstream `database.Driver` registered as
`oceanbase`. Applications keep using golang-migrate/v4 for sources, version
transitions, scheduling, dirty errors and recovery. No root facade, new migration
engine, shared parser framework or database-specific CLI dispatcher is added.

One package and one `oceanbase://` scheme serve both MySQL and Oracle tenants,
with mode auto-detection from native metadata. Separate public packages would
duplicate connection ownership and configuration; wrapping the existing Oracle or
upstream MySQL drivers would carry different client and database assumptions.
A mismatch between an explicit mode in the URL and the server is rejected by the
native client.

Non-goals: SQL dialect translation, automatic reconnect or write replay, tenant
or user creation, forced cascading deletion, and any claim beyond the tested
server/proxy combinations.

## 2. Dependency

`github.com/cocosip/go-oceanbase v0.1.1`, pinned with no local `replace`. It is a
pure-Go client speaking the native protocol, with native server metadata,
`sql.Out` bindings for Oracle PL/SQL and negotiated multi-statements. It
contains no lock logic; `DBMS_LOCK` and `GET_LOCK` are server features called
through ordinary SQL. The client's own Oracle live report (OceanBase 4.4.2.2,
direct connection) passed PL/SQL OUT binds, procedures, cancellation and XA but
does not exercise `DBMS_LOCK`.

## 3. Connection, ownership and session invariants

| File | Responsibility |
|---|---|
| `config.go` | Migration options, native DSN delegation, connection safeguards |
| `oceanbase.go` | Registration, ownership, native metadata, pinned session, errors |
| `lock.go` | Native lock lifecycle and commit/rollback ownership probe |
| `version.go` | Mode-specific state table and transactional history writes |
| `sql.go` | MySQL batch preflight and Oracle statement framing |
| `drop.go` | Scoped catalogs, dependency preflight, supported object cleanup |

The URL is parsed with `x-migrations-table` and `x-statement-timeout` removed and
`allowMultiStatements=true` enforced. Read-only routing and database auto-creation
are rejected. `WithInstance` takes ownership of the pool only on success; failure
discards the reserved session and leaves the pool caller-owned. The pool must be
dedicated, idle and in autocommit.

One physical connection is reserved for metadata, execution and locking. Native
`MetadataServerInfo` supplies the mode and `ob_version()`; handshake MySQL
compatibility versions are not interpreted as OceanBase versions. Validation
requires a nonempty target schema and autocommit, and an initial native
transaction round trip rejects caller-owned active transactions before any
lock probe could commit them. The schema must not change for the life of the
driver. In MySQL mode, a nonzero `lower_case_table_names` lowercases the history
table name; in Oracle mode the name is uppercased.

The session is discarded on context cancellation, network failure, uncertain
commit or a broken invariant. No lock or migration is replayed after a connection
change. Native errors stay in `database.Error.OrigErr`.

## 4. Locking

MySQL uses `GET_LOCK(name, 0)`/`RELEASE_LOCK`; Oracle uses `DBMS_LOCK.REQUEST`
(exclusive mode 6, timeout 0, `release_on_commit => FALSE`) and `RELEASE` with a
numeric ID in `0..1073741823`. Namespaces are tenant-scoped; mode, exact schema
and history table form the hashed lock identity, so a hash collision can
serialize otherwise independent migrations. Waiting is a 100 ms poll bounded by
the caller context, `StatementTimeout` and Close.

Before state-table DDL the driver takes the lock, commits, verifies ownership,
rolls back and verifies again. MySQL compares `IS_USED_LOCK` with
`CONNECTION_ID()`; Oracle `REQUEST` must report already-held (4). A failed probe
or an uncertain lock outcome discards the session. There is no process-lock or
transaction-lock fallback and no automatic downgrade.

### `release_on_commit` documentation finding

The sources are the OceanBase documentation repository `oceanbase/oceanbase-doc`;
go-oceanbase has no `DBMS_LOCK` code. The English V4.3.5 and V4.4.2 REQUEST pages
(headed `machine-translation: en`) say the parameter "can only be set to TRUE"
while their own example passes `FALSE`. The Chinese originals of those versions
say `TRUE` and `FALSE` are both supported, default `FALSE`, and English V5.0.1
was corrected to match. The conflict is a translation error, not a server
limitation. Return values (0 success, 1 timeout, 2 deadlock, 3 parameter error,
4 already held, 5 invalid handle) and RELEASE (0, 3, 4, 5) match the driver's use.
`TRUE` locks are transaction-scoped and cannot be released with `RELEASE`, so
they cannot protect DDL migrations. Never switch to `TRUE` to obtain a successful
call.

Oracle mode is documented as Enterprise Edition only, and the open-source server
tree has no Oracle `DBMS_LOCK` implementation to inspect. In the open-source tree
the MySQL functions are session-owned (`OUT_TRANS_LOCK`, release returns 1) and
require a supporting OBProxy; an unsupported proxy returns "not supported", which
fails initialization. The startup probe still rejects transaction-scoped
behavior, and the migration account needs `EXECUTE` on `DBMS_LOCK`. Source
reading is not live evidence.

References:

- [REQUEST, English V4.3.5](https://github.com/oceanbase/oceanbase-doc/blob/V4.3.5/en-US/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/150.request-of-oracle-mode.md)
- [REQUEST, Chinese V4.4.2](https://github.com/oceanbase/oceanbase-doc/blob/V4.4.2/zh-CN/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/150.request-of-oracle-mode.md)
- [REQUEST, English V5.0.1](https://github.com/oceanbase/oceanbase-doc/blob/V5.0.1/en-US/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/150.request-of-oracle-mode.md)
- [DBMS_LOCK overview](https://github.com/oceanbase/oceanbase-doc/blob/V4.3.5/en-US/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/100.dbms-lock-overview-oracle.md)
- [RELEASE](https://github.com/oceanbase/oceanbase-doc/blob/V4.3.5/en-US/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/210.release-oracle.md)
- [MySQL lock functions](https://github.com/oceanbase/oceanbase/blob/master/src/sql/engine/expr/ob_expr_lock_func.cpp)

## 5. Version state

The history table has a non-null integral primary-key `version` and a `dirty`
column constrained to 0 or 1; the DDL is mode-specific. State changes run in one
transaction (delete, then insert) and preserve upstream `NilVersion` and dirty
`-1` semantics. An unknown commit outcome discards the session. Existing history
is validated (columns, types, nullability, one row) and is never reset, rebuilt
or dirty-cleared automatically; extra columns are rejected.

## 6. SQL framing and preflight

SQL runs as written in the selected dialect, bounded by `StatementTimeout` for the
whole file. MySQL sends the entire preflighted script as one native
multi-statement. Oracle runs ordinary statements without the trailing semicolon
and PL/SQL units (`BEGIN`, `DECLARE`, `CREATE` of function, procedure, package,
trigger or type) terminated by a semicolon and a standalone `/` line.

The tokenizer keeps semicolons opaque inside literals, quoted identifiers,
comments, Oracle alternative quoting (`q'[...]'`) and, in MySQL, according to
`NO_BACKSLASH_ESCAPES` and `ANSI_QUOTES`. Rejected: executable comments
(`/*!`, `/*M!`), nested or unclosed comments, client commands (`\`, `@`,
`DELIMITER`, `SOURCE`, ...), `USE`, `CONNECT`, `ALTER SESSION/SYSTEM`, top-level
`SET`, transaction and lock commands, and Oracle inline `WITH FUNCTION/PROCEDURE`.

Top-level statements are checked by their first word. Inside procedure,
function, trigger, event and anonymous-block bodies only transaction control
(`COMMIT`, `ROLLBACK`, `SAVEPOINT`, MySQL `START`/`XA`) and MySQL `SET` of
session/system variables are rejected, so loops, `EXIT` and local-variable `SET`
remain valid. MySQL bodies are delimited by `BEGIN`/`CASE` ... `END` nesting
because `DELIMITER` is unavailable; this is word-level, not a full MySQL parser.
The driver re-checks schema and autocommit before and after `Run` and before
state writes. Preflight is not an SQL sandbox: dynamic SQL and routine side
effects are not interpreted.

## 7. Drop boundary

`Drop` binds to the original schema, checks visible dependencies from other
schemas (foreign keys and view/object references), drops local foreign keys, then
removes tables, views, routines and triggers (Oracle also sequences, synonyms,
packages and types, ordered by `ALL_DEPENDENCIES`, tables with `PURGE`). It never
drops a tenant, user or database, never cascades by force and never disables
foreign-key checks. Rejected before any DDL: Oracle materialized views, MySQL
events and any unknown object type. Visibility of external references depends on
catalog privileges, and arbitrary routine-body dependencies are not represented.

## 8. Delivery and verification

`cmd/migrate-plus` imports the driver; registration and help output are tested.
Unit tests cover configuration, framing, preflight, locks, state, errors and Drop
with sqlmock; they do not prove wire-level or real-server behavior.

Integration tests are separate per mode and run only against an explicitly
disposable schema (never a production schema), with `MIGRATE_PLUS_OCEANBASE_MYSQL_TEST_URL`
or `MIGRATE_PLUS_OCEANBASE_ORACLE_TEST_URL`. They cover upstream conformance,
lock contention and survival, durable state, dirty/Force/Down, timeout discard,
history reopen and Drop. They destroy the schema's supported objects.

Required local checks are `go build ./...`, `go test ./... -count=1`,
`golangci-lint run ./...`, `go mod verify`, `go mod tidy -diff`,
`git diff --check`, plus the existing pure-Go six-platform builds and Linux race
job.

Unverified until run on real servers: lock behavior per mode (Oracle
`DBMS_LOCK` availability, `EXECUTE` privilege and `FALSE` survival across DDL;
MySQL functions and OBProxy support; `IS_USED_LOCK` against `CONNECTION_ID()`),
session-variable probes in Oracle mode, `lower_case_table_names` and catalog
column visibility, legacy history column types, identifier case, inline `WITH`
routines, Oracle materialized-view backing objects, and wire-level paths (OUT
binds, negotiated multi-statements, cancellation, session loss) that need a
protocol-peer test. Record the server version, mode, direct or proxy path and
package privileges with every result, and preserve failures until the driver,
server or privilege cause is established.
