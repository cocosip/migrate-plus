# openGauss Driver Design

Scope: the `database/opengauss` driver's dependency decisions, server
capabilities, connection ownership, locking, version state, SQL and Drop
boundaries, and verification plan. Usage and operational instructions are in
the [driver guide](../database/opengauss/README.md).

## 1. Scope and decisions

Implement `database/opengauss` as an independent upstream
`github.com/golang-migrate/migrate/v4/database.Driver`, using the official
openGauss client selected by the user:
[openGauss-connector-go-pq](https://gitcode.com/opengauss/openGauss-connector-go-pq).
Register only `opengauss`; applications continue to use upstream constructors,
sources, migration operations and dirty-state recovery. The distributed CLI
includes the driver. openGauss and Huawei GaussDB are different products on
different support lines; they use separate drivers, and neither is a fallback
for the other. See the [GaussDB design](GAUSSDB_DESIGN.md).

openGauss is PostgreSQL-derived (9.2-era kernel lineage). The user selected the
official Go client and asked for the same session-ownership guarantees the
GaussDB driver already provides. The implementation therefore mirrors the
GaussDB driver's structure — one reserved session for migration SQL, state and
locking, no reconnect after fatal errors, strict migration state, catalog-driven
Drop without CASCADE — and changes only what the client or product requires.

The initial implementation enables **A and PG compatibility modes with
client_encoding UTF8** and valid UTF8 SQL files. B/C modes are rejected until
their SQL framing is established. It checks `pgxc_node` when present and rejects
coordinator topologies until cluster-wide lock scope has been established.

The following are design defaults, not additional user requirements:

- Use the client's `database/sql` connector (`NewConnectorConfig` over
  `ParseConfig`); do not depend on its global `sql.Register` names.
- Use native openGauss metadata, parameters, locking and object definitions.
  Compatibility modes do not select the Oracle, DM or PostgreSQL driver.
- Prefer the client's existing whole-script execution path over a new SQL
  statement splitter. Keep framing and protocol restrictions explicit.
- Make small capability branches only for demonstrated server differences.
  No common database base class, dialect translator or migration engine.

MogDB and other openGauss distributions are not support claims; the server
banner must identify openGauss. Plain PostgreSQL is rejected with a pointer to
the upstream postgres driver.

## 2. Dependency findings and version policy

Research on 2026-10-19 inspected repository head at the release commit of
`v1.0.9`, the same commit the `v1.0.8` and `v7.0.0` tags point to. The project
moved its declared module path from `gitee.com/opengauss/openGauss-connector-go-pq`
to `gitcode.com/opengauss/openGauss-connector-go-pq` in 2025; the gitee path
stops at `v1.0.7` and the official README points at gitcode. The pinned
dependency follows the official current path.

| Item | Source finding / design decision |
|---|---|
| Repository | `opengauss/openGauss-connector-go-pq` (official, gitcode) |
| Declared module | `gitcode.com/opengauss/openGauss-connector-go-pq` |
| Pinned dependency | `gitcode.com/opengauss/openGauss-connector-go-pq v1.0.9` |
| Native package name | `opq` (package `pq`) |
| Connector API | `NewConnector(dsn)` / `NewConnectorConfig(*Config)` + `sql.OpenDB` |
| URL parsing | `ParseConfig` accepts `postgres://`, `postgresql://`, `opengauss://`, `mogdb://` |
| SQL driver registration | `opengauss`, `mogdb` (not relied upon) |
| Parameters | Positional `$1`, `$2`, ... |
| Client baseline | `go.mod`: Go 1.13; lib/pq heritage |
| License | MIT inherited from lib/pq (`'pq' Contributors`, Blake Mizerany) |

The client is pure Go: no cgo source files, and its only national-crypto
dependency is `github.com/tjfoc/gmsm` used as a hash function (`sm3.New()` per
call, no shared state) inside the RFC 5802-style SHA256/SM3 authentication.
`golang.org/x/crypto` covers the SHA256 challenge. The Kerberos support is a
pure-Go pluggable hook; no GSS provider is registered, so no cgo enters the
build. `CGO_ENABLED=0` builds verify this continuously.

The client resolves through the default Go module proxy and checksum database;
no `replace` or `GOPRIVATE` configuration is required. `v1.0.9` and the newer
`v7.0.0` tag are the same source; when a proxy absorbs `v7.0.0`, upgrading is a
version-bump only. Keep the project's Go 1.26 minimum, Go 1.27 CI and
`CGO_ENABLED=0` release targets. Implementation adds the pinned dependency and
runs `go mod tidy`; unrelated dependency upgrades are outside this change.
Preserve the client's MIT notice in dependency/release attribution.

## 3. Server configuration and capabilities

Initialization runs one metadata round trip resolving `pg_catalog.version()`,
`current_database()`, `current_schema()`, `sql_compatibility`,
`max_identifier_length`, `standard_conforming_strings`,
`transaction_read_only` and `client_encoding`. It then requires:

- the product banner to identify openGauss (case-insensitive). A `gaussdb`
  banner is rejected with a pointer to the GaussDB driver; a PostgreSQL banner
  with a pointer to the upstream postgres driver;
- a non-system target schema (`pg_*`, `information_schema` rejected);
- A or PG `sql_compatibility` and UTF8 `client_encoding` for the SQL preflight;
- a writable session (`transaction_read_only` off);
- the migrations table within the server identifier limit;
- a resolvable `pg_namespace` OID for the target schema;
- absence of distributed coordinators: when `pg_catalog.pgxc_node` exists,
  `node_type = 'C'` rows abort initialization. Standalone openGauss instances
  expose no coordinator rows and pass.

Capabilities are re-resolved before each nonempty file so session changes cannot
invalidate the lexical preflight assumptions.

## 4. Public API, URL and ownership

`Open(url)` parses the URL before the client sees it: scheme must be exactly
`opengauss`; `x-migrations-table` and `x-statement-timeout` are removed from the
query; duplicate parameters, empty migration values, fragments, conflicting
database aliases, read-only `target_session_attrs` and non-off
`default_transaction_read_only` are rejected. The remaining URL is handed to
`opq.ParseConfig`, and the resulting `*opq.Config` to `NewConnectorConfig` +
`sql.OpenDB`. Native parse errors are masked so URL passwords never leak into
error text.

`WithInstance(*sql.DB, *Config)` accepts any pool whose connections speak the
openGauss wire protocol, including pools built from multi-host DSNs by the
caller. It reserves one physical `*sql.Conn` for all driver work. Success
transfers pool ownership: `Close` closes the session and the pool. Failed
construction discards the reserved connection and leaves the caller's pool
open. A nil pool, invalid table name or negative timeout is rejected before any
connection is reserved.

`Config` exposes `MigrationsTable` (default `schema_migrations`, ASCII
identifier, lowercase, at most 63 bytes and within the server limit) and
`StatementTimeout` (applied as a context timeout to every operation, including
whole files and individual lock attempts).

## 5. Locking and failure lifecycle

Lock uses `pg_try_advisory_lock($1::bigint)` on the reserved session, retrying
contention every 100 ms until acquisition, caller timeout, statement timeout or
`Close`. The lock identity comes from upstream
`database.GenerateAdvisoryLockId(database, schema, table)`. Unlock requires the
server to confirm `pg_advisory_unlock` returned true; otherwise held state is
preserved. Reentrant `Lock` and `Unlock` without a lock return the upstream
`ErrLocked`/`ErrNotLocked` sentinels. Initialization acquires the same lock
around state-table setup, bounded by `migrate.DefaultLockTimeout`.

`discard()` marks the driver broken and destroys the reserved physical
connection through `sql.Conn.Raw` returning `driver.ErrBadConn`; `database/sql`
then closes the underlying client connection. After a discard, every method
fails with `sql.ErrConnDone` until the caller constructs a new driver. Errors
classified as fatal are: transport errors (`ErrBadConn`, `ErrConnDone`, EOF,
net.Error), context cancellation/deadline, and native `*opq.Error` with SQLSTATE
class `08` or FATAL/PANIC severity. SQLSTATE class `08` errors from
wire-compatible client pools are matched through the `SQLState() string`
interface, so foreign pools keep connection-failure classification; only the
official client contributes severity. Server-level errors are wrapped in upstream
`database.Error` with the offending query.

## 6. Version state and migration execution

The state table is created under the migration lock as
`CREATE TABLE <schema>.<table> ("version" BIGINT NOT NULL PRIMARY KEY, "dirty"
SMALLINT NOT NULL CHECK ("dirty" IN (0, 1)))`. An existing relation must be an
permanent ordinary table (`relkind 'r'`, `relpersistence 'p'`); unlogged and
temporary tables are rejected. Its `version`/`dirty` columns are validated by
catalog type OIDs (BIGINT primary version, non-null integer dirty). `Version`
rejects corrupt or multiple rows. `SetVersion` commits DELETE + conditional
INSERT inside one transaction, writes dirty state before running each file,
including dirty version -1, and discards the session when a commit is uncertain.

`Run` reads the whole file unchanged, refreshes capabilities, runs the lexical
preflight, then executes the file as one native simple query. Unlike the
GaussDB driver, this client exposes no server transaction-status API on the
reserved session. After successful execution, Run probes with `SAVEPOINT`:
success means the script left a transaction open; SQLSTATE `25P01` means the
session is idle. Other probe errors fail the migration. When the session is not
confirmed idle — a failed script, an open transaction or an uncertain probe —
Run issues a defensive `ROLLBACK` to clear open or aborted transactions, with a
five-second timeout covering the probe and rollback; a confirmed idle session
skips the rollback. Unfinished transactions return an
error so upstream retains dirty state. A failed ROLLBACK discards the session.
Idle probes can log an expected `25P01` error. Explicit
`BEGIN ... COMMIT` pairs remain valid SQL and commit normally.

Do not assume atomic DDL across modes/builds. Earlier statements may already
have committed when a file fails. Repair actual database state before upstream
`force`; force does not undo SQL.

## 7. SQL and protocol boundaries

The preflight is lexical, not a SQL parser. It rejects NUL bytes, non-UTF8
scripts, unterminated quotes/comments/dollar quotes, top-level COPY, psql/gsql
backslash commands, `DELIMITER`, `SOURCE`, `SPOOL`, `PROMPT`, standalone `/`,
and unquoted procedural bodies (`DECLARE`/`BEGIN` blocks outside native
quoting). It understands nested comments, quoted identifiers, ordinary, escape
and dollar-quoted strings, escape-string continuation across newlines and line
comments, and A-mode `q'...'` alternative quotes, using the
session's live `standard_conforming_strings`. A fuzz target covers the
tokenizer.

Protocol tests exercise the official client against an in-memory wire backend,
covering idle, open and aborted transaction cleanup and subsequent session reuse.
SQL mocks cover upstream dirty-state handling and error paths; real-server
integration tests remain separate. Configure lexical settings before migration; scripts must
not change their meaning while a file is being parsed. Avoid changing
authentication, routing, database or lock ownership, including `DISCARD ALL`
and advisory-unlock calls.

## 8. Drop boundary

Drop removes supported tables, views/materialized views, sequences, native
functions/procedures and user-defined types/domains in the original schema,
including the state table. Catalog collection is identical in shape to the
GaussDB driver: `pg_class` relations (extension-owned and index/toast/composite
relations excluded), `pg_proc` routines with identity arguments, `pg_type`
types and domains; `pg_constraint` foreign keys dropped first; a recursive
`pg_depend` walk classifying internal/auto/extension/external dependencies;
topological ordering that refuses cycles and external or extension-owned
dependencies before any DDL runs. Only native `RESTRICT` is used, never
`CASCADE`. Materialized views force a catalog re-list after their removal.
Concurrent catalog changes or a later permission/DDL error can leave partial
cleanup.

## 9. Delivery and verification

Delivered with this design:

- `database/opengauss/` package: `opengauss.go`, `config.go`, `sql.go`,
  `drop.go`, `README.md`, unit tests (`opengauss_test.go`, `config_test.go`,
  `sql_test.go` with fuzzing, `drop_test.go`, `lock_test.go`,
  `regression_test.go`, `protocol_test.go`) and env-gated `integration_test.go`;
- `go.mod`/`go.sum` with the pinned client and `go mod tidy`;
- CLI composition: blank import and doc comment in `cmd/migrate-plus/main.go`,
  registry and composed-help tests in `cmd/migrate-plus/main_test.go`;
- documentation: this design, the package README, root `README.md`,
  `docs/DESIGN.md`, `THIRD_PARTY_NOTICES.md`, and the GaussDB documents'
  openGauss scope statements updated to point here;
- repository checks: `go build ./...`, `go test ./... -count=1`,
  `golangci-lint run ./...`, `git diff --check`, and a `CGO_ENABLED=0` build,
  all with the repository-local caches.

Real-server verification is a separate acceptance item and has not run here.
When an openGauss instance is available, set
`MIGRATE_PLUS_OPENGAUSS_TEST_URL` (and optionally
`MIGRATE_PLUS_OPENGAUSS_TEST_OTHER_URL`) to disposable schemas and run
`go test -tags integration ./database/opengauss -run TestIntegration -count=1 -v`. Record the
exact server build, compatibility mode and topology. Priority evidence to
collect: SHA256 and SM3 authentication against a real server, A and PG mode
script execution including SAVEPOINT returning `25P01` on idle sessions and
unfinished transactions preserving dirty state, idle ROLLBACK severity, `pgxc_node` behavior on
standalone installs, materialized-view relkind values on the 9.2-lineage
kernel, and Drop coverage. Unit tests and SQL mocks do not establish
real-server compatibility.

## 10. Sources and unresolved evidence

Inspected on 2026-10-19:

- client repository README and source at `v1.0.9` (module path, `sql.Register`
  names, `NewConnector`, `ParseConfig`/`ParseURLToMap` URL schemes,
  `QuoteIdentifier`, `*pq.Error` shape, pure-Go audit, gmsm usage in
  `rfcdigest.go`, empty `kerberos/` directory);
- Go module proxy `/@v/list` and `/@v/v1.0.9.info` plus checksum database
  lookup confirming default-toolchain resolution;
- tag list confirming `v1.0.8`/`v1.0.9`/`v7.0.0` share one commit and the
  `v6.0.0`→`v7.0.0` diff being the module rename plus vector-query additions.
- openGauss server source: `src/gausskernel/storage/access/transam/xact.cpp`
  (`RequireTransactionChain` reports SQLSTATE `25P01` outside a transaction)
  and `src/common/backend/parser/scan.l` (`quotecontinue` retains escape mode).

Unresolved until a real server is exercised: SHA256/SM3 authentication in
practice, SAVEPOINT probe behavior and idle-session ROLLBACK severity across modes, `pgxc_node` contents on
standalone openGauss, exact relkind coverage for openGauss-specific relation
kinds, and B/C-mode SQL framing (rejected by design for now).
