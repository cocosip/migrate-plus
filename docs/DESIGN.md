# migrate-plus Architecture & Design

Version: v0.5. Native golang-migrate extensions with Oracle support.

## 1. Purpose

migrate-plus supplies pure-Go database drivers missing from
`github.com/golang-migrate/migrate/v4`, initially Oracle and DM (Dameng).
It also supplies a CLI built from the upstream command layer and selected
drivers. It is an extension module, not a replacement migration framework or a
fork of the upstream core.

Applications continue to import the upstream migrate package. migrate-plus
contributes driver packages implementing upstream interfaces. Upstream owns
migration scheduling, direction, version transitions, dirty-state recovery,
source parsing, prefetching, and graceful stopping.

M1 established the skeleton. M2 adds Oracle through `github.com/sijms/go-ora/v2`
v2.9.0; DM remains future work. Oracle's target compatibility range follows the
client's documented Oracle 10.2+ baseline, not an unverified server certification.

## 2. Constraints

| Item | Decision |
|---|---|
| Module | `github.com/cocosip/migrate-plus` |
| Go | Minimum `go 1.26.0`; CI uses Go 1.27.x |
| Lint | golangci-lint v2.14.0, aligned with the local installation |
| License | MIT, copyright 2026 cocosip; derived CLI notices are separate |
| Upstream dependency | `github.com/golang-migrate/migrate/v4 v4.20.1` |
| Project structure | One Go module; database drivers are individual packages |
| cgo | `CGO_ENABLED=0`; future database client dependencies must be pure Go |
| Language | Documentation, code, comments, and commit messages in English |
| Automation | GitHub Actions calling Go/lint commands directly; no Makefile |
| New source drivers | None; import upstream sources |
| Root library facade | None; consumers use the upstream migrate API |

Do not redefine `database.Driver`, `source.Driver`, registries, `Migration`,
version sentinels, or error types from upstream. Do not add wrapper constructors
or a second migration execution engine. Existing upstream database drivers
remain upstream imports rather than local copies.

## 3. Component Ownership

| Responsibility | Owner |
|---|---|
| `New`, `NewWithSourceInstance`, `NewWithDatabaseInstance`, `NewWithInstance` | Upstream `migrate` |
| `Up`, `Down`, `Steps`, `Migrate`, `Force`, `Drop`, `Version` | Upstream `migrate` |
| Interfaces, driver registry, `NilVersion`, lock errors | Upstream `database` |
| Migration filename parsing, up/down index, source registry | Upstream `source` |
| Local files and embedded filesystems | Upstream `source/file`, `source/iofs` |
| Additional database SQL, URL configuration, locking, version persistence | `database/<name>` packages here |
| Command parsing and dispatch | Small upstream-derived `cli` package here |
| Which database drivers enter a CLI binary | `cmd/migrate-plus` or a consumer's custom command |
| Common database driver conformance tests | Upstream `database/testing` |

Dependencies follow this direction:

```text
Application ---------------------> golang-migrate/v4
     |                                  |
     +--> migrate-plus/database/<name> --+--> upstream database.Driver/registry

migrate-plus CLI ----------------> golang-migrate/v4
     +--> selected driver imports
     +--> upstream source/file
```

Only imported driver packages enter an application's compiled package graph.
This is package selection within one Go module, not a promise that module
metadata for every other driver is absent from Go's module graph.

## 4. Repository Layout

```text
migrate-plus/
├── go.mod                         # Requires the upstream module
├── go.sum
├── database/                      # Additional database drivers
│   ├── oracle/                    # Oracle database.Driver using go-ora v2
│   └── dm/                        # Future: DM database.Driver
├── cli/
│   ├── main.go                    # Public Main(version string)
│   ├── commands.go                # Calls upstream migrate operations
│   ├── log.go                     # Upstream CLI logger
│   └── main_test.go               # Process-based CLI acceptance tests
├── cmd/migrate-plus/
│   └── main.go                    # Imports Oracle and calls cli.Main
├── LICENSE                        # Project MIT license
├── THIRD_PARTY_NOTICES.md          # Derived CLI provenance and license
├── docs/
└── .github/workflows/ci.yml
```

Paths marked future do not exist. There is no local root migrate package,
base `database` API, source implementation, or shared SQL parser. Add common
driver helpers only when multiple implemented drivers actually need them.

## 5. Library Consumption

### URL-based source and database

Applications use normal upstream imports:

```go
import (
    "github.com/golang-migrate/migrate/v4"
    _ "github.com/golang-migrate/migrate/v4/source/file"
    _ "github.com/cocosip/migrate-plus/database/oracle"
)

m, err := migrate.New("file://migrations", databaseURL)
```

The application handles `err`, calls upstream migration methods, and handles
both errors returned by `m.Close()`. The driver registers its URL scheme in the
upstream registry; migrate-plus does not intercept driver lookup.

### Embedded migrations and existing database connections

`iofs` requires an `fs.FS` instance; it is not a registered `iofs://` URL source:

```go
import (
    "io/fs"

    "github.com/golang-migrate/migrate/v4"
    "github.com/golang-migrate/migrate/v4/database"
    "github.com/golang-migrate/migrate/v4/source/iofs"
)

func newMigrator(files fs.FS, db database.Driver) (*migrate.Migrate, error) {
    sourceDriver, err := iofs.New(files, "migrations")
    if err != nil {
        return nil, err
    }
    m, err := migrate.NewWithInstance("iofs", sourceDriver, "driver-name", db)
    if err != nil {
        // This helper owns the source, while the caller still owns db on error.
        _ = sourceDriver.Close()
        return nil, err
    }
    return m, nil
}
```

The caller obtains `db` from a driver's `WithInstance` or URL opener.
Database ownership and close behavior must be documented by that driver. For a
URL database and embedded source, use upstream `NewWithSourceInstance` instead.

## 6. Implementing an Additional Database Driver

Follow the upstream [database extension instructions](https://github.com/golang-migrate/migrate/blob/v4.20.1/database/driver.go).
Each `database/<name>` package must:

1. Implement upstream `database.Driver`, with a compile-time interface assertion.
2. Register its URL scheme using upstream `database.Register` in `init()`.
3. Implement `Open` and document the URL format and supported `x-*` parameters.
4. Provide `WithInstance` for an existing database client when appropriate.
5. Implement database-specific `Lock`, `Unlock`, `Run`, `SetVersion`, `Version`,
   `Drop`, and `Close`. All configuration comes from the URL or explicit config.
6. Reuse upstream `database/testing.Test(t, driver, migrationSQL)` and add
   database-specific tests for URL handling, connection ownership, locking,
   failure recovery, and SQL execution.
7. Add the driver import to CLI composition only after implementation.

Register directly with the upstream registry, for example in the future DM
package:

```go
var _ database.Driver = (*Dm)(nil)

func init() {
    database.Register("dm", &Dm{})
}
```

Here `database` is `github.com/golang-migrate/migrate/v4/database`; `Dm` is not
implemented. The upstream conformance suite accepts a fresh driver and a
database-specific `[]byte` migration script, and drops database contents at the
end. Run it only against dedicated test databases when implementing a driver.

## 7. Migration State, SQL, Context, and Logging

Upstream selects migrations and drives `SetVersion(version, true)`, `Run`, then
`SetVersion(version, false)`. A driver persists version and dirty state durably
and provides migration exclusion through its database lock. A failed migration
must not silently clear dirty state. The default version-table name should
remain `schema_migrations` unless a particular driver documents a necessary
exception; support a validated `x-migrations-table` override when implemented.

SQL execution and transaction capabilities differ by database. Oracle/DM script
delimiters and procedural blocks belong in the respective driver. The project
does not add a universal SQL splitter or promise transactional DDL across databases.

The upstream interfaces remain unchanged. They do not accept context arguments.
Future drivers may use explicit operation timeout configuration internally, but
must not claim end-to-end caller cancellation through unchanged upstream APIs.
Use upstream `GracefulStop` and `migrate.Logger`; no new logger facade or error
hierarchy is needed. Applications can provide their own logger implementation.

## 8. CLI Composition

The upstream CLI's implementation lives in `internal/cli` and cannot be imported
from this module. Port only its command layer into public `cli` with source
provenance and its MIT notice. It imports the upstream migrate module for every
migration operation. See [third-party notices](../THIRD_PARTY_NOTICES.md).

`cmd/migrate-plus/main.go` calls `cli.Main(Version)`. Driver import files belong
to that command rather than to the reusable `cli` package. A custom binary can
blank-import any compatible upstream or external database driver and call the
same entrypoint. A prebuilt upstream executable cannot load Go packages as
runtime plugins; new drivers must be compiled into a binary.

The distributed CLI registers native `source/file` and the Oracle driver. The retained commands
are `create`, `goto`, `up`, `down`, `drop`, `force`, and database `version`.
`-version` prints the build version. `-path` expands to a file source URL.
Create flags remain upstream `-ext`, `-dir`, `-seq`, `-digits`, `-format`, `-tz`.
There is no invented `-allow-dirty`, `-seq-digits`, or `-print` CLI option.

The supported database set and default CLI imports will be selected as actual
drivers become available. Do not add build-tag files for missing packages or
claim that optional tags alone remove unconditional imports.

## 9. Testing and CI

M1 tests CLI help/build-version/create behavior and command failure without a
database. There are no tests that only revalidate the upstream migration engine.

GitHub Actions runs Go 1.27 tests on Linux/Windows, builds for Linux/macOS/Windows
on amd64/arm64, and runs golangci-lint v2.14.0. All builds disable cgo. No database
services are provisioned by the default workflow. Oracle integration tests are
explicitly enabled against a disposable schema and reuse upstream's conformance
suite. SQL-boundary unit tests run without a database.

## 10. Milestones

| Phase | Scope | Acceptance |
|---|---|---|
| M1 skeleton | Native upstream dependency, composed CLI, driver onboarding docs, CI | Database-free tests/build/lint pass; no duplicate framework |
| M2 Oracle | Native go-ora v2 driver, version capabilities, CLI import | Unit checks pass; real-server conformance remains explicit validation |
| M3 further databases | Add another missing pure-Go driver | Each driver works with the upstream library and composed CLI |
| M4 distribution | Release binaries, Docker packaging, driver matrix | Supported database set and pure-Go build targets are verified |

## 11. Maintenance and Remaining Decisions

Pin and periodically update the upstream module. Review upstream command-layer
changes separately for the small CLI port, recording local changes in
`THIRD_PARTY_NOTICES.md`. Do not vendor the migration core or upstream drivers.

Oracle's decisions are recorded below. DM's client and database-specific behavior
remain undecided. The project uses its own [MIT license](../LICENSE);
the derived command-layer files retain their [third-party notices](../THIRD_PARTY_NOTICES.md).

## 12. Oracle Driver

- `database/oracle` implements upstream `database.Driver` and registers `oracle`.
  `WithInstance(*sql.DB, *Config)` accepts a go-ora pool; successful construction
  transfers pool ownership to the driver. Failed construction leaves the pool
  with the caller and discards its reserved session.
- Connections use go-ora v2.9.0. Read actual server version and `COMPATIBLE` once
  through `DBMS_UTILITY.DB_VERSION`; do not require access to `V$` views. Reject
  server or compatibility versions below 10.2. Allow 128-byte migration table
  identifiers only when both versions are at least 12.2; otherwise allow 30.
  Keep portable SQL shared and add capability branches only for real differences.
  Version state uses `NUMBER(19)` and numeric dirty flags across all versions.
- Accept `x-migrations-table` (an unquoted ASCII Oracle identifier, normalized to
  uppercase) and `x-statement-timeout` (positive milliseconds). Pass native
  connection parameters through to go-ora; reject unknown migration parameters.
- Reserve one `*sql.Conn` for the entire driver lifetime. Use `DBMS_LOCK.REQUEST`
  with `release_on_commit => FALSE` and zero wait, returning upstream `ErrLocked`
  on contention. Derive the numeric lock ID from schema and table using upstream
  lock-ID generation, within Oracle's allowed range. Hash collisions can cause
  unnecessary exclusion, never concurrent migration of the same version table.
  The account needs `EXECUTE ON SYS.DBMS_LOCK` and `DBMS_UTILITY`, plus privileges
  and quota needed by its migrations. Initialization locks version-table creation.
- Persist version changes with transactional `DELETE` and optional `INSERT`;
  preserve dirty `NilVersion`. Never use `TRUNCATE` for state changes because
  Oracle DDL commits implicitly. Migration failure retains upstream dirty state;
  successful earlier DDL cannot be rolled back automatically.
- Parse ordinary SQL at semicolons outside literals and comments. PL/SQL blocks
  and stored units retain internal semicolons and are separated by a slash on
  its own line. Inline `WITH FUNCTION` / `WITH PROCEDURE` SQL also requires a
  slash boundary; remove its final SQL semicolon while preserving inner ones.
  Preserve quoted identifiers, escaped strings and Oracle q-quotes.
  This is driver-local statement framing, not SQL rewriting or SQL*Plus execution.
  User SQL must target its actual Oracle version; no automatic dialect conversion.
- `Drop` removes supported user-schema objects, including tables and their
  dependent indexes/triggers, views, materialized views, sequences, routines,
  packages, synonyms and types. It does not drop the Oracle user, database,
  grants, jobs or database links. Close and reopen after dropping.
- Unit tests cover capabilities, URL/identifier validation, SQL framing, lock
  contention, cleanup, state rollback and execution failures. Opt-in integration
  tests cover upstream conformance, independent sessions, DDL lock survival and
  upstream dirty/force/up/down behavior. Never run destructive tests in a shared
  schema. No Oracle version is marked verified without a real-server test result.

Implementation order: test and implement version/configuration and SQL framing;
test and implement the native driver and failure cleanup; compose the CLI and
update usage documentation; run build, tests, lint and pure-Go cross-builds.
