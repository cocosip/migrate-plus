# GaussDB Driver Design

Status: implemented, 2026-10-15. The native driver, dependency, CLI import,
unit/protocol tests and opt-in server tests are present. Real-server validation
remains outstanding; no Huawei GaussDB build is certified.
This document extends [the project architecture](DESIGN.md) and follows the
structure of the [Oracle](ORACLE_DESIGN.md) and [DM](DM_DESIGN.md) designs.

## 1. Scope and decisions

Implement `database/gaussdb` as an independent upstream
`github.com/golang-migrate/migrate/v4/database.Driver`, using the Huawei client
selected by the user:
[database-gaussdb-go](https://github.com/huaweicloud-samples/database-gaussdb-go).
Register only `gaussdb`; applications continue to use upstream constructors,
sources, migration operations and dirty-state recovery. The distributed CLI
includes the driver. See the [usage guide](../database/gaussdb/README.md).

The user selected Huawei GaussDB and asked for the supported server range to
follow that client's documented support. The client README states **GaussDB
2.x and higher**. Use that as the compatibility target, rather than inventing
a particular minimum patch, limiting the design to one supplied instance, or
claiming every version/topology is already certified.

The initial implementation enables **A and PG modes with client_encoding UTF8**
and valid UTF8 SQL files. Other modes are rejected. It checks `pgxc_node` when
present and rejects coordinator topologies until cluster-wide lock scope has
been established. This narrows enabled capabilities, not the client's stated
server baseline. No alternate database driver is used as a fallback.

The following are design defaults, not additional user requirements:

- Use the client's `database/sql` adapter and the existing dedicated-session
  ownership contract.
- Use native GaussDB metadata, parameters, locking and object definitions.
  Compatibility modes do not select the Oracle, DM or PostgreSQL driver.
- Prefer the client's existing whole-script execution path over a new SQL
  statement splitter. Keep framing and protocol restrictions explicit.
- Make small capability branches only for demonstrated server differences.
  No common database base class, dialect translator or migration engine.

GaussDB(for MySQL), PostgreSQL and openGauss are not additional support claims.
The client repository's use of openGauss in CI does not certify Huawei GaussDB
server versions or distributed deployments for this project.

## 2. Dependency findings and version policy

Research on 2026-10-15 inspected release `v1.0.0-rc1`, commit
`1eccdbac5e7790f8c13668fd2506c8b712c12993`, and repository head
`a3f2b15334db92063b5a8b661b8af9ad7300c3b7`. GitHub and the public Go module
proxy expose `v1.0.0-rc1` as the available release; it is a prerelease.

| Item | Source finding / design decision |
|---|---|
| Repository | `huaweicloud-samples/database-gaussdb-go` |
| Declared module | `github.com/HuaweiCloudDeveloper/gaussdb-go` |
| Pinned dependency | `github.com/HuaweiCloudDeveloper/gaussdb-go v1.0.0-rc1` |
| Native package name | `gaussdbgo` |
| SQL adapter | `github.com/HuaweiCloudDeveloper/gaussdb-go/stdlib` |
| SQL driver registration | `gaussdb` |
| Parameters | Positional `$1`, `$2`, ... |
| Client baseline | README: Go 1.22+ and GaussDB 2.x+; `go.mod`: Go 1.21 |
| License | MIT, with the client archive's copyright notice |

The old module URL resolves to the selected repository. Import the declared
module path; do not use the repository's new name as an incompatible module
path or add a `replace` merely to hide the difference.

The inspected client is pure Go and provides `stdlib.OpenDB`, `GetConnector`,
`*stdlib.Conn.Conn`, context operations, transactions and server SQLSTATE errors.
The relevant connection, adapter, configuration, error and simple-query source
files are unchanged between the inspected release and head. Start with the
release; do not follow an unpinned branch. If a required client bug fix is found,
review and pin a specific newer release or pseudo-version with a regression
case, rather than silently changing client implementations.

Keep the project's Go 1.26 minimum, Go 1.27 CI and six `CGO_ENABLED=0` release
targets. Source inspection is not a substitute for those builds. Implementation
adds the pinned dependency and runs `go mod tidy`; unrelated dependency upgrades
are outside this change. Preserve the client's MIT notice in dependency/release
attribution without changing the project's license.

## 3. Server configuration and capabilities

Reserve one `*sql.Conn` before initialization and resolve the actual connected
server and namespace. The implementation uses these native sources:

| Information | Source | Use |
|---|---|---|
| Product/build | `SELECT version()` | Diagnostics and tested build records; do not compare its PostgreSQL ancestry version with GaussDB 2.x |
| Database and schema | `current_database()`, `current_schema()` and `pg_namespace` | Freeze the actual version-table namespace; reject a null/missing schema |
| Compatibility and framing | `current_setting('sql_compatibility')`, `standard_conforming_strings`, `client_encoding` | Accept only known A/PG and UTF8 framing; refresh before each nonempty Run |
| Identifier bound | `SHOW max_identifier_length` | Validate the state-table name before creating it |
| Read/write state | Native transaction/session read-only setting | Reject a migration session that cannot write |
| Required lock functions | Native catalogs plus actual acquire/release | Fail initialization if required locking is unavailable or forbidden |

These server-side queries are not certified statements for every GaussDB 2.x+
build. Their names, catalog columns, return types, permissions and mode
differences still require opt-in server verification. Never fall back to
PostgreSQL version rules or Oracle catalogs on a permission error.

The client README does not give an exhaustive mode/topology matrix. Support
the same native migration operations wherever GaussDB provides the required
capabilities; record each exercised mode and deployment separately. Unknown
product identity or missing required capabilities must produce an actionable
initialization error. A banner alone is not reliable proof of topology or a
complete compatibility guarantee.

Keep capability data private and limited to actual decisions. A script must
not change the connection's role, database, migration schema, lock ownership or
session routing. Resolve all state-table operations against the original
schema even if user SQL changes `search_path`; close and reopen to change the
migrator's namespace.

## 4. Public API, URL and ownership

Follow the two existing drivers:

```go
package gaussdb

const DefaultMigrationsTable = "schema_migrations"

type Config struct {
    MigrationsTable string
    StatementTimeout time.Duration
}

func WithInstance(instance *sql.DB, config *Config) (database.Driver, error)

// GaussDB implements database.Driver; init registers "gaussdb".
```

A nil config selects defaults; copy configuration rather than modifying it.
Do not initially add a native-connection constructor, a second pool API,
schema aliases or query-mode configuration to the migration API.

```text
gaussdb://user:password@host:5432/app?search_path=app&sslmode=verify-full&x-migrations-table=schema_migrations&x-statement-timeout=30000
```

The path is the database name; `search_path` is a native session parameter, not
a database name. The example assumes the client's normal certificate setup.
Retain native TLS, connection timeout, application name and multi-host settings.
Multi-host fallback is allowed while initially establishing the session; it
does not permit reconnecting and continuing an in-progress migration.

| Migration option | Contract |
|---|---|
| `x-migrations-table` | Default `schema_migrations`; ASCII `[A-Za-z][A-Za-z0-9_]*`, normalized to lowercase and at most the lesser of 63 bytes and the server identifier limit |
| `x-statement-timeout` | Positive integer milliseconds within `time.Duration`; absent / Config zero means no driver operation timeout |

Reject nil pools, negative Config timeouts, malformed/duplicate/empty migration
options, unknown `x-*`, URL fragments and qualified/quoted table overrides.
Validate the URL query before the client parses it; do not let its first-value
handling silently choose between duplicate migration options. Strip migration
options before calling `gaussdbgo.ParseConfig` and use `stdlib.OpenDB` so this
driver explicitly selects the requested adapter. Keep native parameter
semantics; reject conflicting identity aliases or routing settings rather than
silently choosing one. Verify password encoding with `@`, `:`, `/`, `+`, `%`,
`?` and `&`, IPv6 addresses and comma-separated host/port lists.

Native client parsing can use `GAUSSDB_*`, service and password-file defaults.
Do not add migrate-plus environment variables or mutate process-wide client
configuration. Resolve and validate the effective native configuration; record
the actual database/schema after connecting. Validate effective read-only
routing, including settings inherited by the client, rather than checking URL
text alone.

Never return raw credential-bearing DSNs in validation errors. The inspected
client's parse-error redaction hides URL userinfo passwords but retains query
parameters, including a query `password` value. Convert client parse errors to
safe migration-driver diagnostics without wrapping a credential-bearing parse
error. Retain native connection and SQL errors where their messages are safe;
test malformed URL and query-password cases explicitly.

Successful `WithInstance` transfers ownership of the dedicated pool. `Close`
releases and closes its reserved session and the pool. Failed construction
leaves the caller's pool open, discarding a session whose state may be unsafe.
`Open` closes its newly created pool if initialization fails. Shared application
or GORM pools, and adapters backed by `OpenDBFromPool`, are outside the initial
ownership contract. Real instances must use the selected client's direct
`stdlib` adapter; inspect native state only inside `Conn.Raw` callbacks and
never retain raw pointers outside them. Unit tests may use sqlmock.

## 5. Locking and failure lifecycle

The native locking operations are:

```sql
SELECT pg_try_advisory_lock($1::bigint);
SELECT pg_advisory_unlock($1::bigint);
```

GaussDB availability, session lifetime and scope require real-server acceptance.
Use session locks, never transaction-scoped advisory locks, row
locks, a process-local mutex or a silently successful no-op. The lock must
survive commit, rollback and DDL and must exclude other migration sessions.

Derive a numeric ID with upstream `database.GenerateAdvisoryLockId` from the
resolved database, schema and normalized table name, parse it to `int64`, and
bind it as bigint. Different spelling of the same state-table identity must
not produce independent locks. Hash collisions may cause extra contention.

Retry a false try-lock result every 100 ms until acquisition, driver timeout or
`Close`. Local repeated Lock/Unlock calls return upstream sentinels. A false
unlock result or SQL error must not report successful release or clear held
state. Initialization acquires the same lock around table discovery/creation
and validation, then releases it before returning the driver.

Match DM/Oracle timeout semantics: upstream `Migrate.LockTimeout` bounds the
caller's wait but does not cancel `Driver.Lock`; `Close` cancels and waits for
pending acquisition and releases any acquired lock. The driver's configured
timeout bounds the whole attempt, including during initialization. Initialization
additionally uses `migrate.DefaultLockTimeout` (15 seconds), set by the CLI from
`-lock-timeout` before driver construction; the shorter timeout wins. Library
callers configure this default before construction. Cleanup
must preserve both operation and release errors.

**Distributed acceptance is a separate requirement.** Test competing sessions
through different coordinator nodes as well as one endpoint. A function's
presence or same-node exclusion does not prove cluster-wide locking. If native
advisory locks are node-local, that topology is unsupported by this strategy;
do not advertise distributed support or substitute unsafe locking. Establish a
documented cluster-wide primitive before enabling it. A fixed endpoint is not
evidence that all other migration processes use the same coordinator.

All operations use the same reserved session, including `SetVersion` and
`Version`. On a lost session, network/timeout uncertainty or uncertain
commit/rollback, discard the physical connection and require a new driver.
Never acquire a replacement connection or replay migration SQL. Normal SQL
errors need not poison an otherwise idle, live session. Use native connection
closed/transaction status to distinguish these cases; retain native SQLSTATE
through upstream `database.Error` rather than creating a new error hierarchy.

## 6. Version state and migration execution

Native state table created by the implementation:

```sql
CREATE TABLE "app"."schema_migrations" (
    "version" BIGINT NOT NULL PRIMARY KEY,
    "dirty" SMALLINT NOT NULL CHECK ("dirty" IN (0, 1))
);
```

Keep the same numeric dirty contract as DM/Oracle. Quote identifiers with the
client's existing `gaussdbgo.Identifier.Sanitize`, and bind data using `$n`.
The configurable table is an ordinary identifier; catalog-returned schema and
object names require proper quoting even when they contain quotes or Unicode.

Discover the exact schema-qualified relation through native catalogs under
the session lock. Validate an existing table's ordinary relation kind,
non-null BIGINT version with a single-column primary key, non-null integer dirty
column and current data. Existing CHECK expressions are not interpreted;
Version always enforces dirty 0/1 and at most one row. A BOOLEAN dirty column is not
silently reused or converted. A conflicting view or other relation is an error.

`SetVersion` uses one transaction containing DELETE and optional INSERT, not
TRUNCATE or mode-dependent UPSERT. Preserve upstream clean empty state and
dirty `NilVersion` (-1); reject versions below -1. Empty `Version` returns
`database.NilVersion`; invalid dirty values and multiple rows are errors.
Commit dirty state before running user SQL and clean it only after successful
execution with no transaction left open.

The initial `Run` contract is one native simple-query execution per file:

1. Read the entire `io.Reader`; a read failure executes nothing.
2. Refresh mode, string escaping and client encoding on the reserved session;
   reject unsupported settings and perform the protocol/client-command checks below.
3. Require an idle, usable reserved connection.
4. Call `conn.ExecContext(ctx, script)` with no parameters. In the selected
   client this chooses simple query, iterates all results and returns their
   error. An empty or whitespace-only file may succeed.
5. Verify native `TxStatus()` is idle after success. An open or failed explicit
   transaction is an error; roll it back with a fresh cleanup context before
   allowing unlock, recheck idle status, or discard the session if cleanup fails. Never mark such a
   migration clean. On execution error, apply the same transaction cleanup
   before returning the original error together with any cleanup error.

The client documents that simple-query batches are implicitly transactional
unless transaction controls occur. Do not turn that client statement into a
universal atomic-DDL promise for every GaussDB version/mode. Test actual DDL,
DML and explicit-transaction behavior; prior statements may have committed.
The driver does not insert a new whole-migration transaction. Operations that
cannot run in a multi-command transaction require a separate migration file
containing one command if that server supports it.

`StatementTimeout` bounds the entire file execution, each internal metadata/state
operation, and the complete lock attempt. This differs from DM/Oracle's
per-framed-statement Run timeout and must be explicit in the usage guide.
Native `statement_timeout` remains a separate server setting. Preserve dirty
state after any Run/commit uncertainty; recovery remains upstream repair/force.

## 7. SQL and protocol boundaries

Send native SQL unchanged; let GaussDB parse statement boundaries, quoted
strings, comments and supported procedural bodies. Ordinary multi-statement
SQL and server-supported dollar-quoted functions/DO blocks require no client
semicolon splitting. Compatibility-specific routines must be verified on
GaussDB; Oracle/DM slash framing is not automatically applicable here.

This is a SQL-file API, not a gsql, psql or SQL*Plus interpreter. Client
backslash commands, variable substitution, include/source commands, `DELIMITER`
and standalone slash terminators are unsupported. Document supported native
examples instead of automatically translating these scripts.

**COPY protocol needs a guard.** The selected simple-query result loop does
not service COPY input. Passing `COPY ... FROM STDIN` unchanged can wait for
input instead of producing an ordinary SQL error. Reject interactive COPY
commands before execution, including server-file COPY, COPY TO STDOUT and psql `\copy`; no COPY data
stream API is part of this driver. Do not reject ordinary text inside literals,
comments or supported routine bodies as though it were a client command.

Implement only the lexical preflight needed for this boundary in a local
`sql.go`: recognize top-level COPY commands and client-only directives while
respecting verified string, identifier, comment and routine-body quoting.
Keep it separate from statement execution; do not split or rewrite SQL. Its
lexical rules follow the actual mode and effective string settings where
they differ. Byte scanning is limited to UTF8 client sessions and valid UTF8 files;
non-UTF8 client encodings are rejected. Both CR and LF end line comments.
Unsupported/unknown lexical forms must fail before execution;
never claim the preflight is a complete SQL validator. If this cannot be made
reliable for a mode, that mode needs a documented script restriction and must
not be silently treated as PostgreSQL. This check must be tested before
whole-script execution is shipped.

Scripts must leave transactions balanced and must not release migration locks
or change authentication/routing. The idle-status check catches an unfinished
transaction; it does not prove the meaning of arbitrary user SQL or prevent
trusted migration SQL from deliberately unlocking a session.

## 8. Drop boundary

Follow the existing original-schema boundary. Drop the migration table and
supported ordinary/partitioned tables, views, materialized views, sequences,
functions/procedures and user-defined types/domains, including dependent local
indexes, constraints and triggers where native object semantics remove them.
Do not drop the database, user/role, schema, extensions, system objects, grants,
jobs or objects in other schemas.

Do not implement `DROP SCHEMA ... CASCADE` followed by schema recreation: it can
remove external dependents and change ownership/privileges. Do not copy
Oracle's PURGE/FORCE, DM catalogs, or upstream PostgreSQL's table-only cascade
as a claim of complete GaussDB cleanup.

Validate the relevant native catalogs and object-type distinctions for the
actual server first. Preflight unsupported classes, extension ownership,
external dependencies and unresolvable dependency cycles before issuing DDL.
Mode-specific packages, synonyms or independently represented triggers require
documented discovery/drop rules; otherwise their presence makes Drop fail
explicitly before deletion. Do not pretend unsupported objects were removed.

Use a fixed object-type allowlist, quote qualified names, and obtain overloaded
routine identities from the server's supported identity function/catalog.
Remove local foreign-key dependencies and order other local objects from
dependents to prerequisites. Use RESTRICT/default restrictive native behavior;
never use broad CASCADE to get around an external dependency. Handle
materialized views before re-listing remaining tables/sequences so retained
backing objects are included. An owned sequence already removed with its table
must not be treated as a fresh Drop failure.

Reject system namespaces at construction. Stop on actual Drop errors; earlier
DDL may already have committed. Concurrent object changes can invalidate the
preflight, so restrictive DROP behavior remains necessary. Close and reopen
after Drop. Cross-schema dependencies, quoted names, overloads and objects
owned by another role require real-server tests.

## 9. Delivery and verification

Implementation reaches the following locations:

| Location | Implementation work |
|---|---|
| `database/gaussdb/` | `config.go`, `gaussdb.go`, limited `sql.go` preflight, `drop.go`, corresponding unit tests, lock tests, opt-in integration tests and README |
| `go.mod`, `go.sum` | Pinned Huawei client and reviewed transitive dependencies |
| `cmd/migrate-plus/main.go` and tests | Blank import; registry/help checks for all three drivers |
| `README.md`, `docs/DESIGN.md` | Implemented scope, package layout and usage links; preserve the distinction from verified server support |
| `THIRD_PARTY_NOTICES.md` / release packaging | Client provenance and MIT notice |
| Existing CI/release jobs | Reuse unit/lint/module/race/six-target builds; no default database installation |

The native adapter protocol tests establish whole-file execution, I/T/E
transaction status and rollback behavior without SQL mocks. SQL mocks establish
the driver's catalog/state/locking decisions. Neither establishes actual Huawei
server SQL support. Opt-in server tests remain necessary for metadata, advisory
lock lifetime/scope, state DDL and Drop catalogs. Record missing native primitives
as limitations rather than inventing a fallback.

Unit tests use the existing sqlmock dependency and cover configuration,
credential redaction, namespace binding, ownership, lock retries and release
failures, initialization races/cleanup, dirty -1, corrupt history, transaction
failures and broken-session rejection. Add lexical fixtures for COPY guards
and native script forms, including COPY text inside quoted bodies. The native
idle-status boundary requires a focused adapter check as well as SQL mocks;
sqlmock alone cannot establish protocol or TxStatus behavior.

Opt-in integration tests use `MIGRATE_PLUS_GAUSSDB_TEST_URL`, unset by default,
and an empty disposable schema. Reuse upstream `database/testing.Test`. The
suite is destructive and also checks upstream up/down/steps/force/dirty
behavior, two-pool exclusion through DDL/commit/rollback, timeouts, multi-statement
errors, DML durability, open/failed transactions, native routines and Drop's
second-schema boundary. Reconnect and COPY rejection are also covered locally.

Record exact Huawei GaussDB build, compatibility mode, deployment/coordinator
topology, client revision, session settings and results. Exercise older and
recent available releases before claiming compatibility across release
families. Treat different-coordinator locking and failover as separate cases;
openGauss tests and SQL mocks do not replace Huawei GaussDB results. The present
matrix has no verified entries.

Implementation validation follows the existing repository checks:
`go test -count=1 ./...`, `go build ./...`, `go vet ./...`,
`golangci-lint run ./...`, `go mod verify`, `go mod tidy -diff`,
`git diff --check` and all six pure-Go CLI builds. The existing Linux CI job
also owns race detection; local Windows checks do not establish Linux runtime behavior.

Local validation on 2026-10-15 used Go 1.27.2 on Windows and golangci-lint
2.14.0: all ordinary tests, build, vet, lint, module verification/tidiness,
six pure-Go CLI targets and a five-second SQL-preflight fuzz smoke run passed.
GaussDB statement coverage was 87.6%. Server tests skipped because their URL
was unset. Additional Windows race checks could not start (0xc0000139), also
reproduced with an empty Go program; they do not provide a race-test result.

## 10. Sources and unresolved evidence

- [Selected client repository](https://github.com/huaweicloud-samples/database-gaussdb-go)
- [Release README and support declaration](https://github.com/huaweicloud-samples/database-gaussdb-go/blob/v1.0.0-rc1/README.md#supported-go-and-gaussdb-versions)
- [Declared module and dependency versions](https://github.com/huaweicloud-samples/database-gaussdb-go/blob/v1.0.0-rc1/go.mod)
- [Public module proxy release metadata](https://proxy.golang.org/github.com/%21huawei%21cloud%21developer/gaussdb-go/@v/v1.0.0-rc1.info)
- [SQL adapter, registration and ownership primitives](https://github.com/huaweicloud-samples/database-gaussdb-go/blob/v1.0.0-rc1/stdlib/sql.go)
- [Native execution, identifiers and configuration](https://github.com/huaweicloud-samples/database-gaussdb-go/blob/v1.0.0-rc1/conn.go)
- [Simple-query execution, result loop and transaction status](https://github.com/huaweicloud-samples/database-gaussdb-go/blob/v1.0.0-rc1/gaussdbconn/gaussdbconn.go)
- [Native DSN, TLS, runtime settings and fallback parsing](https://github.com/huaweicloud-samples/database-gaussdb-go/blob/v1.0.0-rc1/gaussdbconn/config.go)
- [Native errors and password-redaction limits](https://github.com/huaweicloud-samples/database-gaussdb-go/blob/v1.0.0-rc1/gaussdbconn/errors.go)
- [Client MIT license](https://github.com/huaweicloud-samples/database-gaussdb-go/blob/v1.0.0-rc1/LICENSE)
- [Client CI's openGauss test target](https://github.com/huaweicloud-samples/database-gaussdb-go/blob/v1.0.0-rc1/.github/workflows/ci.yml)
- [Huawei GaussDB documentation linked by the client](https://doc.hcs.huawei.com/db/en-us/index.html)
- [Upstream database interface](https://github.com/golang-migrate/migrate/blob/v4.20.1/database/driver.go)

No real GaussDB server was accessed. The client source establishes Go APIs and
wire execution paths, not the validity of server metadata, advisory
locks or Drop queries. Huawei Cloud support pages returned a security
verification page during research; their SQL/manual content was not verified.
Availability and distributed scope of advisory locks, exact compatibility
capabilities, native catalog/Drop SQL and actual transaction behavior remain
real-server acceptance gates. They are not confirmed support claims.
