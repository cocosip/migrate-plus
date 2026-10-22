# DM driver

Native [golang-migrate](https://github.com/golang-migrate/migrate) database driver
using `gitee.com/chunanyong/dm v1.8.23`. It registers the `dm` URL scheme.

See the [DM driver design](../../docs/DM_DESIGN.md) for implementation decisions
and the [project architecture](../../docs/DESIGN.md) for shared contracts.

## Compatibility

The target is DM8 across multiple builds. Initialization identifies the product
from `V$VERSION` and reads the actual schema, case sensitivity and effective
`BACKSLASH_ESCAPE`. Missing functions, privileges or unsupported settings fail
initialization. Lock and migration lifecycle tests have been run on DM8
8.1.4.80 in Oracle compatibility mode. This does not certify every feature:
the native Drop test exposed a separate catalog-filter problem. See the
[lock compatibility finding](../../docs/DM_DESIGN.md#lock-compatibility-finding).

`BACKSLASH_ESCAPE` is a static INI parameter. Initialization reads its active
memory value with `SF_GET_PARA_VALUE(2, 'BACKSLASH_ESCAPE')`, not the on-disk
value or `SF_GET_SESSION_PARA_VALUE` (which can fail with error -842 because
the parameter is not session-scoped). On native connections this value must
match `DmConnection.BackSlashFlag`, received in the server login response.
A query failure, non-boolean value or disagreement stops initialization;
neither compatibility mode nor a default value substitutes for this check.

| Server `COMPATIBLE_MODE` | Target | Real-server verification |
|---|---|---|
| 0 | Native DM | Not run |
| 2 | Oracle compatibility | Lock/migration lifecycle passed on 8.1.4.80; native Drop test failed |
| 4 | MySQL compatibility | Not run |
| 7 | PostgreSQL compatibility | Not run |

All modes use DM-native metadata, SQL and locking. These modes do not implement
the corresponding database completely. The client's separate `compatibleMode`
option accepts 0/1/2 in this client release; it does not select server mode.
The driver changes neither setting and does not rewrite migration dialects or
select migration directories. Write numbered SQL for the actual DM configuration.
DM7 and cluster/failover-wide locking are outside the verified scope.

## Usage

```go
import (
    "github.com/golang-migrate/migrate/v4"
    _ "github.com/golang-migrate/migrate/v4/source/file"
    _ "github.com/cocosip/migrate-plus/database/dm"
)

m, err := migrate.New("file://migrations", "dm://user:password@localhost:5236?schema=APP")
// Handle err, call m.Up(), and handle both errors from m.Close().
```

The distributed CLI includes this driver:

```sh
migrate-plus -path ./migrations -database 'dm://user:password@localhost:5236?schema=APP' up
```

Native connection parameters pass through without URL decoding/re-encoding.
Use the native client's raw credential rules; percent escapes are not decoded
by its DSN parser. `/APP` is also accepted as a schema path. Conflicting path
and query schemas, duplicate options and unknown `x-*` parameters are rejected.
Do not log connection URLs. Actual special-character credential connections
still require real-server verification.

| Native DSN option | Meaning |
|---|---|
| `schema=APP` or `/APP` | Requests the initial schema; the driver still queries its actual identity for the migration table and lock |
| `compatibleMode=0/1/2` or `oracle/mysql` | Client behavior only: 0 default, 1 Oracle, 2 MySQL; passed through unchanged, not the server's `COMPATIBLE_MODE` |
| `escapeProcess=false` | Disables client SQL escape rewriting; distinct from the server's `BACKSLASH_ESCAPE` |

This client release has no DSN option for `BACKSLASH_ESCAPE` or database case
sensitivity. DSN values cannot replace these server facts. `WithInstance`
receives an already-open pool rather than a DSN, so the same initialization
checks apply to both entry points.

| Migration option | Default | Accepted values |
|---|---|---|
| `x-migrations-table` | `schema_migrations` | ASCII identifier, up to 128 bytes; normalized uppercase |
| `x-statement-timeout` | Disabled | Positive integer milliseconds |

`WithInstance(*sql.DB, *Config)` accepts a dedicated native DM pool. A nil config
selects defaults. Success transfers pool ownership; driver `Close` closes it.
Failure leaves the caller's pool open and discards the reserved physical session.
Do not pass a shared GORM/application pool that must remain usable afterward.

The pool must use `autoCommit=true`, `rwSeparate=0`, `doSwitch=0`,
`driverReconnect=false`, `maxRows=0`, `enRsCache=false` and `escapeProcess=false`.
Row limits can hide corrupt state or Drop objects; result caching can return
stale versions, and escape processing rewrites SQL. URL opening supplies these
explicitly and rejects
conflicts. For `WithInstance` they are caller preconditions: the client exposes
no complete public runtime configuration getter. Do not enable other filters or
global configuration that reconnect, replay or route statements to another
session. One reserved session holds metadata operations, SQL and the lock.

## Database prerequisites

The migration account needs access to `V$VERSION`, `SYS.SYSOBJECTS`,
`SYS.SYSCONS`, `SYS.SYSDEPENDENCIES`, and these native functions:
`CURRENT_SCHID`, `SF_GET_SCHEMA_NAME_BY_ID`, `SF_GET_PARA_VALUE`,
`SF_GET_CASE_SENSITIVE_FLAG`.
Grant the specific access needed on the deployed build, plus privileges and
quota required by the scripts; the driver does not require a blanket DBA role.

`DBMS_LOCK.REQUEST` and `RELEASE` require execution privileges. If the package
is absent, an administrator can install it using
`SP_CREATE_SYSTEM_PACKAGES(1, 'DBMS_LOCK')`. The driver never installs packages
or grants itself permissions. Lock requests and releases use SELECT plus Scan,
binding only the lock ID, to avoid an anonymous-block mixed OUT/IN binding
failure reproduced on the tested build. Native lock requests use zero wait; the driver
retries contention every 100 ms. Locks survive commit, rollback and DDL.
`Migrate.LockTimeout` (CLI `-lock-timeout`) bounds the caller's wait; upstream
does not retry or cancel `Driver.Lock`. Always call `Close` after a lock timeout
to cancel pending acquisition and release any acquired lock.
`x-statement-timeout` bounds the whole driver lock attempt, including retries.
Initialization additionally uses `migrate.DefaultLockTimeout` (15 seconds),
which the CLI sets from `-lock-timeout` before opening the driver. A shorter
statement timeout takes precedence. Library callers must set the default before
construction; setting an instance's `LockTimeout` afterward cannot affect setup.
Repeated `Lock` calls on the same driver return
`database.ErrLocked`.

Do not change the current schema, compatibility mode, escaping or routing
settings during a migrator's lifetime, including indirectly through routines.
Close and reopen after configuration changes. Timeout or connection-loss
uncertainty terminates the session; construct a new migrator and inspect the
persisted dirty state. No SQL is replayed. DDL and successful earlier SQL may
already be committed; migrations are not promised atomic rollback.

## Existing migration state

An existing `SCHEMA_MIGRATIONS (VERSION BIGINT, DIRTY BIT)` table is accepted
without alteration or initialization writes, but only as a permanent table:
DM temporary tables are rejected through `SYSOBJECTS.INFO3`, because
transaction-scoped temporary rows vanish on commit and session-scoped rows on
disconnect. `BIT` is allowed only for `DIRTY`;
`VERSION` still requires `BIGINT` or an integral numeric column with at least
19 digits of precision. Existing integer/numeric `DIRTY` columns remain supported.
Only 0/false and 1/true are accepted; NULL, invalid values and multiple rows fail.
The native client returns BIT as `int8`, integer columns as signed integer widths,
and integral DECIMAL values as strings; boolean dirty values are also handled.
An empty table means `database.NilVersion`. Existing dirty history remains dirty
and upstream `Up` refuses it until the caller explicitly resolves that state.

## SQL scripts

Ordinary SQL uses semicolons outside single/double quotes and comments. Doubled
quote escaping is supported; backslash handling follows the active
`BACKSLASH_ESCAPE` value checked against the native connection. Native procedural
blocks and stored units (including CLASS/CLASS BODY) use a slash on its own line:

```sql
CREATE TABLE items(id BIGINT PRIMARY KEY);
CREATE OR REPLACE PROCEDURE items_noop AS
BEGIN
    NULL;
END;
/
INSERT INTO items VALUES(1);
```

The final procedural block may end at EOF. Internal and final block semicolons
are preserved. The complete input is framed before execution. Syntax validation
still belongs to DM. Oracle q/nq quotes, MySQL backticks, PostgreSQL dollar
quotes, nested comments, inline WITH routines and client commands such as
`DELIMITER`, `SOURCE`, `COPY`, `PROMPT`, `@` and backslash commands are rejected.
`SET` is limited to transaction/constraint statements. This is native SQL
framing, not DIsql, SQL*Plus, mysql or psql emulation.

## Drop

Upstream `Drop` is destructive and includes the version table. It removes
supported objects in the original migration schema: tables, views, materialized
views, sequences, synonyms, functions, procedures, packages, types, classes and
standalone triggers. Native table cleanup handles dependent indexes/triggers.
Foreign keys in that schema are removed first; materialized views are removed
before re-reading remaining objects to account for backing tables.

External dependencies and cycles in the complete supported-object graph are
checked before destructive DDL, including foreign-key removal. Unresolved
object cycles require explicit cleanup SQL; local foreign-key cycles are removed
by dropping their constraints. No `CASCADE`, Oracle `PURGE` or `FORCE` is used.
Catalog/DDL errors stop processing; earlier DDL may have committed. Users,
schemas, databases, grants, jobs, links and global system packages are excluded;
this is not a general schema eraser. Close and reopen after Drop. Native catalog
classification and dependency behavior need verification on every tested build.

## Integration tests

Default tests use mocks and need no server. The following tests **destroy
supported objects in the selected schema**; use a disposable schema exclusively.

```powershell
$env:MIGRATE_PLUS_DM_TEST_URL = 'dm://test_user:password@localhost:5236?schema=TEST_USER'
go test -count=1 -v ./database/dm -run '^TestIntegration$'
```

Tests cover existing BIT/integer version tables (logging actual dirty return
types), clean/dirty preservation, SetVersion round trips, upgrade/repeated Up,
upstream conformance, dirty/force/down, DML durability, lock survival
across transactions/DDL, timeout termination, routines and native Drop ordering.
Set `MIGRATE_PLUS_DM_TEST_MATERIALIZED=1` to also test ordinary/prebuilt
materialized views on builds/accounts supporting them. Set
`MIGRATE_PLUS_DM_OTHER_TEST_URL` to another disposable schema on the same server
with cross-schema SELECT privileges to test external dependency refusal and
sentinel preservation. Missing optional prerequisites are reported as skipped.

Run against an older and a recent DM8 build in each target mode; record exact
build, case, escaping, DDL commit settings and results before claiming tested
compatibility. See [design and source references](../../docs/DM_DESIGN.md).

For a standalone database/sql comparison of legacy SELECT, bound SELECT and
anonymous-block calls, set `MIGRATE_PLUS_DM_LOCK_PROBE_DSN` to a native DM DSN
and run `go test -count=1 -v ./database/dm -run '^(TestLockCompatibility|TestLockProbeMetadata)$'`.
These diagnostics use a fresh physical session for every case, perform no
DDL/Drop, and stop at the first error in each case. The diagnostic command is
expected to fail for incompatible anonymous-block cases; it is not the normal
regression suite. Do not set its DSN when running the ordinary test suite.
