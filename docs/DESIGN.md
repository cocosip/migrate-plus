# migrate-plus Architecture & Design

Scope: project architecture, shared contracts, CLI composition and validation.
Database-specific designs are maintained separately.

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

Oracle, DM8, GaussDB and openGauss drivers are implemented and included in the distributed CLI.
Their detailed designs, compatibility targets and verification boundaries are
listed in [Driver designs](#10-driver-designs).

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
│   ├── dm/                        # DM8 database.Driver using the native DM client
│   ├── gaussdb/                   # GaussDB database.Driver using Huawei's native client
│   └── opengauss/                 # openGauss database.Driver using the official pq-fork client
├── cli/
│   ├── main.go                    # Public Main(version string)
│   ├── commands.go                # Calls upstream migrate operations
│   ├── log.go                     # Upstream CLI logger
│   └── main_test.go               # Process-based CLI acceptance tests
├── cmd/migrate-plus/
│   └── main.go                    # Imports Oracle/DM/GaussDB/openGauss and calls cli.Main
├── LICENSE                        # Project MIT license
├── THIRD_PARTY_NOTICES.md          # Derived CLI provenance and license
├── docs/
│   ├── DESIGN.md                  # Project architecture and shared contracts
│   ├── ORACLE_DESIGN.md           # Oracle-specific driver design
│   ├── DM_DESIGN.md               # DM-specific driver design
│   ├── GAUSSDB_DESIGN.md          # GaussDB-specific driver design
│   └── OPENGAUSS_DESIGN.md        # openGauss-specific driver design
└── .github/workflows/ci.yml
```

There is no local root migrate package,
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

Register directly with the upstream registry, for example in the DM
package:

```go
var _ database.Driver = (*Dm)(nil)

func init() {
    database.Register("dm", &Dm{})
}
```

Here `database` is `github.com/golang-migrate/migrate/v4/database`; `Dm` is
implemented in `database/dm`. The upstream conformance suite accepts a fresh
driver and a database-specific `[]byte` migration script, and drops database contents at the
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

Both library drivers and the CLI are supported deliverables. Library consumers
do not need the CLI; command-line users receive a binary with the selected
drivers already registered. Reuse upstream migration APIs, source drivers,
database drivers, command behavior, and flags wherever available.

The upstream CLI's implementation lives in `internal/cli` and cannot be imported
from this module. Port only its command layer into public `cli` with source
provenance and its MIT notice. It imports the upstream migrate module for every
migration operation. See [third-party notices](../THIRD_PARTY_NOTICES.md).

`cmd/migrate-plus/main.go` calls `cli.Main(Version)`. Driver import files belong
to that command rather than to the reusable `cli` package. A custom binary can
blank-import any compatible upstream or external database driver and call the
same entrypoint. A prebuilt upstream executable cannot load Go packages as
runtime plugins; new drivers must be compiled into a binary.

The distributed CLI registers native `source/file`, Oracle, DM, GaussDB and openGauss drivers. The retained commands
are `create`, `goto`, `up`, `down`, `drop`, `force`, and database `version`.
`-version` prints the build version. `-path` expands to a file source URL.
Create flags remain upstream `-ext`, `-dir`, `-seq`, `-digits`, `-format`, `-tz`.
There is no invented `-allow-dirty`, `-seq-digits`, or `-print` CLI option.

The supported database set and default CLI imports will be selected as actual
drivers become available. Do not add build-tag files for missing packages or
claim that optional tags alone remove unconditional imports.

For a selected database already supported upstream, import its upstream driver.
For an additional database such as Oracle or DM, implement `database.Driver` in
`database/<name>` and import it from `cmd/migrate-plus`. Adding a database must
not require a separate command dispatcher or database-specific branches in
`cli`. The CLI help lists compiled drivers: Oracle, DM, GaussDB and openGauss are included in the
distributed CLI; real-server verification
remains a separate acceptance item.

## 9. Testing and CI

M1 tests CLI help/build-version/create behavior and command failure without a
database. There are no tests that only revalidate the upstream migration engine.

GitHub Actions runs Go 1.27 tests on Linux/Windows/macOS, verifies module integrity
and tidiness, collects coverage, builds CLI artifacts for Linux/macOS/Windows on
amd64/arm64, and runs golangci-lint v2.14.0. All builds and normal tests disable
cgo; a separate Linux race-detector run enables cgo. CodeQL scans Go on primary
branch pushes, pull requests, and weekly. Dependabot checks modules and Actions
weekly. Pushing a `v*` tag reuses CI before publishing six CLI archives with
embedded tag versions, project license, CLI notices, README, and SHA-256 checksums.
Tags containing `-` are prereleases. No database
services are provisioned by the default workflow. Oracle, DM, GaussDB and openGauss integration tests are
explicitly enabled against a disposable schema and reuse upstream's conformance
suite. SQL-boundary unit tests run without a database.

## 10. Driver designs

Use the same documentation structure for every implemented database driver:

| Driver | Detailed design | Usage and operational guide |
|---|---|---|
| Oracle | [ORACLE_DESIGN.md](ORACLE_DESIGN.md) | [database/oracle/README.md](../database/oracle/README.md) |
| DM | [DM_DESIGN.md](DM_DESIGN.md) | [database/dm/README.md](../database/dm/README.md) |
| GaussDB | [GAUSSDB_DESIGN.md](GAUSSDB_DESIGN.md) | [database/gaussdb/README.md](../database/gaussdb/README.md) |
| openGauss | [OPENGAUSS_DESIGN.md](OPENGAUSS_DESIGN.md) | [database/opengauss/README.md](../database/opengauss/README.md) |

This document owns upstream integration, package boundaries, common migration
contracts, CLI composition and CI. Each driver design owns dependency decisions,
server/version capabilities, connection ownership, locking, state SQL, framing,
Drop behavior and database-specific verification boundaries. Each package README
owns connection examples, account setup, script examples and test commands.

When adding a driver, add its design and usage guide together and link them
here. Keep database-specific details out of the project architecture. Update
compatibility claims only with the corresponding test evidence; no driver
currently has real-server certification.

## 11. Maintenance and Remaining Decisions

Pin and periodically update the upstream module. Review upstream command-layer
changes separately for the small CLI port, recording local changes in
`THIRD_PARTY_NOTICES.md`. Do not vendor the migration core or upstream drivers.

The project uses its own [MIT license](../LICENSE). Derived command-layer files
retain their [third-party notices](../THIRD_PARTY_NOTICES.md); database client
provenance and terms are recorded separately from the project license.
