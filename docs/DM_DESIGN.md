# DM Driver Design

Status: updated 2026-10-16. DM8 8.1.4.80 Oracle-mode lock and migration lifecycle
verified; native Drop has a separate failing integration test. See the
[validation report](DM_LOCK_COMPATIBILITY.md) for exact scope and limits.
This document extends [the project architecture](DESIGN.md). Connection examples,
account setup and test commands belong to the [DM driver guide](../database/dm/README.md).

## 1. Scope and decisions

Implement `database/dm` as a native
`github.com/golang-migrate/migrate/v4/database.Driver`, backed by
`gitee.com/chunanyong/dm`. Register `dm` in the upstream registry and import the
package from `cmd/migrate-plus`. Applications continue to use
upstream migrate constructors, sources and migration operations.

The user selected DM8 across multiple versions for the first release. Cover
native, Oracle, MySQL and PostgreSQL compatibility modes as separate DM test
configurations. A DM compatibility mode does not turn DM into that other
database and does not guarantee its SQL syntax, data types, catalogs, procedural
language, transaction behavior or advisory locks.

The design selects these implementation defaults:

- One DM driver, with private capability checks for actual DM differences.
- DM-native metadata and locking operations in all modes wherever verified.
- Existing CLI commands and flags; no DM-specific command dispatcher.
- User-authored numbered SQL for the actual DM version and configuration.
- No dialect conversion, automatic choice of a different database driver,
  automatic server reconfiguration, new migration engine, or DM7 claim.

## 2. Dependency findings and version policy

Research inspected the `v1.8.23` module archive, source commit
`8ba262af5d7ddcd733eb21e7485f8831689d8c28`, and the DM documentation linked below.
The Go module proxy returned `v1.8.23` as latest on 2026-10-15. Its README and
`p.go` identify the client release as originating from DM 8.1.4.200.

The package registers `sql.Register("dm", ...)`, supports `sql.Out`, positional
`?` binding, `BeginTx`, `Commit`, `Rollback`, and context-based execution. Its
source contains no `import "C"`; pure-Go builds are included in the project's six release targets.

Neither the client release number nor its protocol floor certifies a server
range. The source checks a protocol version floor of `7.0.0.9`; the README's
`8.1.1.126` reference identifies installations carrying the Go driver, rather
than a complete server compatibility guarantee. Target DM8, beginning with
8.1.x releases available for testing. Record exact tested server builds; do not
advertise all DM8 builds as verified. Versions without the required native
functions or locking package fail initialization with an actionable error.

At implementation time, run the requested `go get -t -u ./...` and
`go mod tidy` after adding the client; inspect the resulting direct and
transitive dependency changes. Keep the existing Go 1.26 minimum and Go 1.27 CI.
The inspected archive has no LICENSE file and its Go files carry Dameng
copyright notices. Keep dependency provenance distinct from this project's MIT
license; document available dependency terms rather than attributing MIT to it.

## 3. Server configuration and capabilities

There are two different compatibility settings:

| Setting | Meaning | Values relevant here |
|---|---|---|
| Server `COMPATIBLE_MODE` | DM SQL compatibility behavior | 0 native, 2 Oracle, 4 MySQL, 7 PostgreSQL |
| Client DSN `compatibleMode` | Client-side compatibility behavior | Client enum 1 Oracle, 2 MySQL in v1.8.23 |

Do not derive server behavior from the client enum, infer it from a server
version, or pass server value 7 to the client's limited enum. Do not automatically
set either setting. Unknown or other server modes are unsupported initially and
must be rejected explicitly.

During initialization, reserve one `*sql.Conn`, then obtain:

| Information | Authoritative source | Use |
|---|---|---|
| Product version/build | Product/database version banners in native `V$VERSION` | DM8 identification and build-specific capability checks |
| Client-reported version | Snapshot `*dm.DmConnection.ServerVersion` through `Conn.Raw` | Protocol diagnostics; do not assume it equals the product version |
| Server compatibility mode | `SF_GET_PARA_VALUE(2, 'COMPATIBLE_MODE')` | Read the active in-memory configuration |
| Current schema identity | `CURRENT_SCHID()` and `SF_GET_SCHEMA_NAME_BY_ID(...)` | Canonical state-table namespace and lock identity |
| Database identifier case behavior | `SF_GET_CASE_SENSITIVE_FLAG()` | Metadata lookup tests and collision handling |
| Effective string escaping | `SF_GET_PARA_VALUE(2, 'BACKSLASH_ESCAPE')`, cross-checked with client `BackSlashFlag` | SQL statement framing |
| DDL commit behavior | Client `DDLAutoCommit` snapshot and native parameter documentation | Transaction expectations and test selection |

The DM8 parameter table classifies `BACKSLASH_ESCAPE` as static (0 off, 1 on).
`SF_GET_SESSION_PARA_VALUE` only accepts session-scoped INI parameters; using it
for this setting can fail initialization with error -842. Read scope 2 (memory),
not scope 1 (the INI file, which may contain a pending restart change).
In client v1.8.23, `BackSlashFlag` is assigned from a byte in the server login
response (`zq.go`), and used when processing string literals (`m.go`, `zzk.go`).
It is a connection-login snapshot, not derived from DSN `compatibleMode`.
The DSN parser has no `BACKSLASH_ESCAPE` or case-sensitivity option; its `schema`
option requests a schema but cannot provide the actual numeric schema identity.
Keep these server checks for both URL opening and already-open pools.
Resolve any memory/handshake disagreement before framing SQL; fail
initialization if a required behavior cannot be determined. Permission errors
must not silently choose an assumed mode or an Oracle fallback.

These contracts are based on the published DM8 manual and the pinned client
source; they are not a verified matrix of historical DM8 builds or modes.
No server-version branch or exception-driven fallback is introduced. Verify
the parameter contract and actual literal behavior on each deployed build.

Distinguish the DM product build from database-format and message/protocol
versions. Do not reject a DM8 server merely because a protocol version begins
with 7, or treat the client's bundled 8.1.4.200 release as the connected server's
version. The account needs read access to the specific version metadata used;
document that requirement rather than asking for a DBA role.

Copy scalar values inside `Conn.Raw`; do not retain the raw connection outside
the callback or use reflection/unsafe to read private client configuration.
These are private implementation details, not a public capability framework.
Use one common SQL path until a documented and tested difference requires a
small branch. A version branch must identify the affected operation and include
a regression fixture; do not invent a distinct implementation for every release.

The mode is not the entire dialect. Case sensitivity, string escaping and DDL
commit settings are independent inputs. Do not change those settings to make a
migration work. Scripts and administrators must not change the schema, mode or
framing-related settings during one migrator's lifetime; close and reopen after
such a change. Keep version-table operations bound to the original schema.

## 4. Public API, URL and ownership

Use the existing driver shape:

```go
package dm

type Config struct {
    MigrationsTable string
    StatementTimeout time.Duration
}

func WithInstance(instance *sql.DB, config *Config) (database.Driver, error)

// Dm implements database.Driver; init registers "dm".
```

A nil config selects defaults. Accept:

```text
dm://user:password@host:5236?schema=APP&x-migrations-table=schema_migrations&x-statement-timeout=30000
```

`schema` remains a native client option, not a migration database name. Also
retain the native DSN schema path form, but reject conflicting path and query
schema specifications. Read the actual schema after connection establishment.

Only two migration options are initially supported:

| Option | Contract |
|---|---|
| `x-migrations-table` | Default `schema_migrations`; normalize to uppercase; ASCII `[A-Za-z][A-Za-z0-9_$#]*`, at most 128 bytes |
| `x-statement-timeout` | Positive integer milliseconds; absent means no driver timeout |

The identifier restriction applies to the driver's configurable state table,
not to names appearing in user SQL. Quote the resolved schema, table and state
column names with double quotes, escaping embedded double quotes in catalog
names. Bind data values using `?`; never interpolate them into SQL. Use the
server-returned schema ID and normalized table name for lock identity so case
aliases cannot migrate the same state table under different locks.

Reject duplicate migration options, malformed values and unknown `x-*` options.
Strip only migration options, preserving other native query text and credential
bytes instead of round-tripping the entire DSN through `url.Values.Encode`.
The client's `parseDSN` splits query text itself and does not apply normal
`net/url` decoding. Its README's password-escaping advice must be tested against
the selected release: `escapeProcess` also participates in SQL processing and
must not be enabled as an unverified password fix. Include passwords containing
`@`, `:`, `/`, `+`, `%`, `?` and `&` in DSN tests and actual connection tests;
document/reject forms the native client cannot represent. Do not log raw DSNs.

Require a dedicated migration pool with native `autoCommit=true`, `rwSeparate=0`,
`doSwitch=0`, `driverReconnect=false`, `maxRows=0`, `enRsCache=false` and
`escapeProcess=false`. URL opening rejects conflicting
values and explicitly supplies these values so client configuration files do
not override defaults. Treat client property names case-insensitively when
checking conflicts, and reject ambiguous duplicates. Leave other connection
settings to the client unless they violate a demonstrated driver invariant.

`WithInstance` has the same ownership contract as Oracle: success transfers
ownership of the pool; `Close` releases the session and closes the pool.
Construction failure leaves the caller's pool open and discards the reserved
physical connection if session state may be unsafe. `Open` closes the pool it
created if construction fails. A GORM/application pool must not be shared with
this driver when later application work still needs it.

The native client does not expose all effective pool settings through public
getters. For `WithInstance`, the above pool settings are a documented caller
precondition, not a claim of complete runtime validation. Unit tests may use
sqlmock; real clients must be the selected DM client. No global client settings
may be mutated to configure a single driver instance.

## 5. Locking and failure lifecycle

Use DM's native `DBMS_LOCK` on the reserved session:

```sql
SELECT DBMS_LOCK.REQUEST(?, 6, 0, FALSE) FROM DUAL;
SELECT DBMS_LOCK.RELEASE(?) FROM DUAL;
```

Read each result with `QueryRowContext().Scan()` and bind only the lock ID.
On DM8 8.1.4.80 in Oracle compatibility mode with client v1.8.23, the former
anonymous block mixing an OUT result and an IN lock ID prepares successfully
but fails during execution with error 6001/EOF. SELECT with an IN lock ID works
on a fresh session under the same parameters. See the
[reproduction and validation report](DM_LOCK_COMPATIBILITY.md); this is not a
claim that every anonymous block or every DM8 build has the same defect.
Positional arguments avoid Oracle-specific
parameter names: DM documents `LOCK_ID` and `LOCK_MODE`, whereas the Oracle
driver uses `id` and `lockmode`. Derive a numeric lock ID through upstream
`GenerateAdvisoryLockId` from schema ID and normalized table name, mask to
`0..1073741823`, and do not call `ALLOCATE_UNIQUE`.

The exclusive lock must survive commit, rollback and DDL until explicit release
or physical session termination. REQUEST result 0 succeeds; result 1 is retried
every 100 ms until acquisition, the configured statement timeout, or `Close`.
The statement timeout bounds the whole lock attempt, including initialization.
Upstream `Migrate.LockTimeout` bounds the caller's wait without retrying or
cancelling `Driver.Lock`. Call `Close` after a lock timeout; it cancels and waits
for pending acquisition before releasing the session. Initialization also uses
`migrate.DefaultLockTimeout` (15 seconds), which the CLI sets from `-lock-timeout`
before opening the driver; a shorter statement timeout takes precedence.
Library callers configure this default before construction. Other return values are errors with the native code. In
particular, do not reuse Oracle's interpretation of result 4: DM's published table describes it
as not owning the lock. Local repeated Lock/Unlock calls use upstream lock
sentinels; a failed native release must not clear local locked state.

An administrator may need to install the package with
`SP_CREATE_SYSTEM_PACKAGES(1, 'DBMS_LOCK')` and grant execution access. The driver
never creates system packages or grants itself privileges. A missing package
or privilege is an initialization failure, with no fallback to a row lock or
process-local mutex.

Lock version-table discovery/creation as well as upstream migration execution.
Every operation uses the same session; a lost connection aborts the migrator
without acquiring a replacement session or replaying SQL. On connection-loss or
timeout uncertainty, discard the physical connection rather than return it to
the pool; require a new driver and inspection of the persisted dirty state.
`Close` attempts unlock and cleanup, preserving all relevant errors.

Do not assume dedicated `sql.Conn` alone prevents client-side reconnection or
read routing; that is why the native pool settings above are required. Tests
must prove lock exclusion across two independent pools, DDL, commit/rollback,
disconnect and error cleanup. Cluster-wide exclusion is not certified merely
by a single-node test; DSC/MPP and failover topologies need separate validation.

## 6. Version state and migration execution

Create a schema-qualified table with quoted uppercase columns:

```sql
CREATE TABLE "APP"."SCHEMA_MIGRATIONS" (
    "VERSION" BIGINT NOT NULL PRIMARY KEY,
    "DIRTY" SMALLINT NOT NULL
)
```

Use DM-native catalogs scoped to the resolved schema, the documented
`SYS.SYSOBJECTS` table for discovery and `SYS.SYSCONS`/`SYS.SYSDEPENDENCIES`
for Drop, independent of compatibility views. Confirm table columns and
dirty values before using a pre-existing state table; do not silently
claim compatibility with an arbitrary application's migration history.
Legacy `VERSION BIGINT, DIRTY BIT` tables are accepted without alteration or
initialization writes. BIT compatibility applies only to DIRTY; VERSION retains
its 64-bit integral requirement. The native client returns BIT as int8, integers
as int8/int16/int32/int64 and integral DECIMAL as text. Read only exact 0/1 or
false/true dirty states and reject NULL, other values and unsupported types.

`SetVersion` uses a transaction containing DELETE plus optional INSERT. Preserve
upstream dirty `NilVersion` and empty clean state. Do not use TRUNCATE, a
mode-dependent upsert, auto-increment types, or native BOOLEAN. Empty `Version`
returns upstream `NilVersion`; multiple rows or invalid dirty values are errors
instead of an arbitrary first-row result.

Upstream still owns `dirty -> Run -> clean`. Run executes framed statements
in order through the reserved connection and stops at the first failure.
Successful DML must be durable with the required `autoCommit=true`; SetVersion
commit/rollback must restore that setting. Test those transitions with the real
client. Do not wrap the entire migration in a promised atomic transaction:
DDL and explicit transaction statements can commit independently, and DM's
DDL_AUTO_COMMIT is configurable. Failure retains dirty state; earlier SQL may
already have taken effect.

Use per-operation contexts for configured timeouts. The client's cancellation
watcher closes the connection; report this as termination requiring a new
migrator, not as resumable query cancellation. Upstream still has no public
context-aware migration API. SQL errors use upstream `database.Error` and retain
the native error/code; do not introduce an error hierarchy.

## 7. Statement framing across compatibility modes

All modes use the DM driver and DM server. The driver frames statements; it
does not emulate SQL*Plus, DIsql, mysql or psql, nor validate/translate a whole
SQL dialect.

Use a DM-local framer. Reuse the Oracle framer's tested approach where DM
semantics match, but do not make DM import the Oracle database package. Do not
extract a universal parser merely because both drivers recognize semicolons.
A shared private helper is justified only by actual identical code and tests
after both requirements are established.

The initial script contract is:

- Split ordinary SQL on semicolons outside comments, quoted identifiers and
  string literals. Respect effective backslash escaping rather than guessing
  from compatibility mode. Preserve literal and identifier bytes.
- Frame DM anonymous blocks and stored procedures/functions/packages/triggers
  with `/` on a line of its own. Preserve their internal and final semicolons;
  do not send the slash to the server. Permit the final block at EOF.
- Native DM procedural framing remains available in every target mode. A mode
  name does not imply support for Oracle PL/SQL, MySQL stored programs or
  PostgreSQL PL/pgSQL in full.
- Add mode-sensitive quote handling only for syntax documented and demonstrated
  on DM. Oracle alternative quotes, MySQL backticks/backslash rules, and
  PostgreSQL dollar-quoted bodies require separate DM evidence and fixtures;
  copying their behavior from another database is insufficient. Where DM accepts
  such quotes, the framer must preserve embedded semicolons and slash lines.
- Reject recognized unsupported client commands such as `DELIMITER`, `SOURCE`,
  SQL*Plus/DIsql setup directives, `COPY FROM STDIN` framing and psql backslash
  commands. Do not silently execute fragments of their intended script.
- Frame the complete input before executing its first statement. Unterminated
  quotes/comments or an identifiable incomplete procedural envelope are framing
  errors. SQL and procedural syntax validation still belongs to DM; do not
  promise to detect every malformed block without executing it.

Ship the ordinary SQL and native DM procedural forms in all four modes first.
Vendor-style extensions are not advertised until DM accepts them and framing
tests cover them. Invalid SQL still comes from the server; the framer's job is
to avoid unsafe boundaries, not to certify SQL semantics.

The application chooses its numbered SQL set for its actual DM configuration.
Do not automatically choose an `oracle/`, `mysql/` or `postgres/` migration
directory based on DM mode. A future mode-specific migration file convention
would be an application concern and is outside this driver design.

## 8. Drop boundary

Match upstream's destructive Drop semantics within the original migration
schema, including the version table. Initial supported objects are ordinary
tables, views, sequences, synonyms, procedures, functions, packages and types,
plus materialized views where the tested build supports them. Table-dependent
indexes and triggers follow native DM dependency behavior. Account for
standalone triggers if DM catalogs expose them independently.

Generate DM-native DROP syntax from a fixed object-type allowlist, quoting
catalog names and using explicit schema qualification. Do not copy Oracle's
`PURGE`, `FORCE`, catalog columns, or generated-object filters without DM
documentation and tests. Handle dependent objects and materialized view backing
tables in phases; re-query after each phase so prebuilt containers are not
missed. Confirm supported cascade clauses do not delete objects in other
schemas; otherwise fail with a dependency error rather than crossing the schema
boundary. Validate the complete supported-object dependency graph before any
DDL, including local foreign-key removal, so a known cycle leaves constraints
intact. Still re-query after materialized views to handle removed backing tables.
Stop and report errors; no generic catch-and-ignore path.

Do not drop users, schemas, the database, grants, jobs, database links or global
system packages. Do not silently claim complete schema removal for unsupported
object classes. Close and reopen the driver after Drop. Tests must prove a
second schema remains unchanged, including cross-schema dependency cases.

## 9. Delivery and verification

Implementation reaches these locations:

| Location | Change |
|---|---|
| `database/dm/` | Config, native driver, statement framing, Drop, unit tests, opt-in integration tests, README |
| `cmd/migrate-plus/main.go` and tests | Blank-import DM; verify both Oracle and DM registration |
| `cli/` tests | Verify compiled driver help without adding database-specific dispatch |
| `go.mod`, `go.sum` | Selected DM client and reviewed dependency updates |
| `README.md`, `docs/DESIGN.md` | Actual implemented scope and usage; keep implementation distinct from verification |
| `THIRD_PARTY_NOTICES.md` | Dependency provenance/available terms where needed, without changing the project license |
| Existing CI/release workflows | Reuse build/test/lint/release jobs; no default licensed database download |

Implement and validate in this order:

1. Capability/DSN/identifier tests, then native metadata and configuration.
2. SQL framing fixtures for the four server modes and escaping variants.
3. Locking, version persistence, cleanup and initialization failure tests.
4. Native Drop behavior, schema boundary and dependency tests.
5. Real-server integration and upstream conformance, then CLI/docs integration.
6. Full build, unit tests, race tests, lint, module checks and six pure-Go builds.

Unit tests use existing sqlmock and require no database. Cover out parameters,
all lock return codes, rollback/commit failures, dirty -1, multiple state rows,
pool ownership, original-schema binding, timeout termination, malformed SQL and
the literal values that must survive execution. Mock expectations alone do not
prove a DM SQL operation is valid.

Integration tests are enabled only with `MIGRATE_PLUS_DM_TEST_URL` against a
disposable schema. Include upstream `database/testing.Test`, actual upstream
up/down/steps/force/dirty behavior, two-pool lock contention, DDL lock survival,
DML durability, timeout/disconnect, stored units, and Drop dependencies.
Destructive tests must never run against a shared application schema.

Maintain a matrix recording exact server build, mode, database case setting,
effective escaping, DDL commit behavior and results. Target native (0), Oracle
(2), MySQL (4) and PostgreSQL (7) on each release family available for testing.
Exercise both identifier case configurations and DDL/escape settings where
supported, without implying every combination exists on every DM8 build.
Mark unavailable or untested combinations explicitly. Verify at least an older
and a recent DM8 build before claiming support across versions; a failure may
establish a narrower documented minimum.

Required local checks are `go build ./...`, `go test -count=1 ./...`,
`golangci-lint run ./...`, `go mod verify`, `go mod tidy -diff`,
`git diff --check`, and the existing race/cross-build checks. Real-server results
remain a separate acceptance item; implementation and mock tests do not certify
server versions.

## 10. Sources and unresolved evidence

- [DM module release metadata](https://proxy.golang.org/gitee.com/chunanyong/dm/@v/v1.8.23.info)
- [Inspected DM v1.8.23 source archive](https://proxy.golang.org/gitee.com/chunanyong/dm/@v/v1.8.23.zip)
- [DM client repository at v1.8.23](https://gitee.com/chunanyong/dm/tree/v1.8.23):
  `p.go` registration/release, `n.go` DSN/options, `m.go` transactions/cancellation,
  `u.go` out bindings, `zq.go` protocol versions, `zf.go` reconnection.
- [DM Go programming guide](https://eco.dameng.com/docs/zh-cn/pm/go-rogramming-guide.html)
- [DM DBMS_LOCK package](https://eco.dameng.com/docs/zh-cn/pm/dbms_lock-package.html)
- [DM SQL introduction](https://eco.dameng.com/docs/zh-cn/pm/dm_sql-introduction.html)
- [DM SQL definitions and session settings](https://eco.dameng.com/docs/zh-cn/pm/definition-statement.html)
- [DM system functions](https://eco.dameng.com/docs/zh-cn/pm/sql-appendix.html)
- [DM INI parameters and static BACKSLASH_ESCAPE](https://eco.dameng.com/docs/zh-cn/pm/physical-storage.html)
- [DM initialization and case settings](https://eco.dameng.com/docs/zh-cn/pm/dminit-parameters.html)
- [DM dynamic views and mode examples](https://eco.dameng.com/docs/zh-cn/pm/dm8-admin-manual-appendix2.html)
- [Upstream database extension interface](https://github.com/golang-migrate/migrate/blob/v4.20.1/database/driver.go)

The implementation must resolve actual metadata/Drop SQL on each tested build,
session-versus-server parameter availability, supported vendor-style quoting,
credential escaping, and native locking behavior. These require source/manual
checks plus real DM connections; they are not reasons to substitute another
database's semantics. No real DM instance has been exercised. Exact available test builds and connection details can be provided when
integration validation begins.
