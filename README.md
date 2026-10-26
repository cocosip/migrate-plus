# migrate-plus

Additional pure-Go database drivers for
[golang-migrate/migrate](https://github.com/golang-migrate/migrate), plus a CLI
that composes those drivers with the upstream migration library.

Applications keep using `github.com/golang-migrate/migrate/v4`. A migrate-plus
driver implements the upstream `database.Driver` and registers with the upstream
registry. The migration engine, sources, file parsing, errors, and public library
API come from golang-migrate; they are not implemented again here.

The project provides both library drivers and a CLI. Applications import driver
packages and keep using the upstream migrate API without depending on the CLI.
Command-line users install `migrate-plus`, which retains upstream commands and
flags and compiles in the selected database drivers. Existing upstream drivers
are reused by import when selected; additional drivers belong to this project.

## Current status

Oracle is implemented with `github.com/sijms/go-ora/v2 v2.9.0`. Its target range
follows go-ora's documented Oracle 10.2+ baseline. The driver reads the server
version and `COMPATIBLE` at connection time to select supported capabilities.
Real-server verification is separate from that compatibility target; no server
version has been verified by this repository yet.

DM8 is implemented with `gitee.com/chunanyong/dm v1.8.23`, targeting native,
Oracle, MySQL and PostgreSQL compatibility modes through DM-native operations.
No DM server build has been verified yet. See the [DM driver guide](database/dm/README.md)
for prerequisites, mode distinctions, SQL restrictions and opt-in tests.

GaussDB is implemented with Huawei's native pure-Go client
`github.com/HuaweiCloudDeveloper/gaussdb-go v1.0.0-rc1`. The client targets
GaussDB 2.x+; this driver initially accepts A/PG modes without distributed
coordinators. No Huawei server build has been verified yet. See the
[GaussDB driver guide](database/gaussdb/README.md) for SQL restrictions and tests.

openGauss is implemented with the official pure-Go client
`gitcode.com/opengauss/openGauss-connector-go-pq v1.0.9` (a lib/pq fork with
openGauss SHA256/SM3 authentication). The driver initially accepts A/PG
compatibility modes without distributed coordinators. No openGauss server build
has been verified yet. See the [openGauss driver guide](database/opengauss/README.md)
for connection strings, SQL restrictions and tests.

Go 1.26 or newer is required; CI uses Go 1.27.x with `CGO_ENABLED=0`.
The distributed CLI includes Oracle, DM, GaussDB, openGauss, OceanBase and the upstream file source.

OceanBase MySQL and Oracle tenants are supported by one driver using the native
pure-Go client `github.com/cocosip/go-oceanbase v0.1.5`, with mode auto-detection.
No OceanBase server build has been verified yet. See the
[OceanBase driver guide](database/oceanbase/README.md) for connection strings,
lock requirements, SQL restrictions and opt-in tests.

```sh
go run ./cmd/migrate-plus -help
go run ./cmd/migrate-plus -version
go run ./cmd/migrate-plus create -ext sql -dir ./migrations -seq init
go run ./cmd/migrate-plus -path ./migrations -database 'oracle://user:password@localhost:1521/service' up
```

`-version` prints the binary build version. The `version` command queries a
configured database's migration version.

`goto V` accepts versions from 0 through the platform's signed integer maximum.
`-lock-timeout N` also bounds the native drivers' initialization lock wait
(default 15 seconds). Library callers can set `migrate.DefaultLockTimeout`
before constructing drivers; a shorter driver statement timeout still applies.

## Library integration

For repeatable real-server regression after driver changes, use the repository's
[configurable migration test program](docs/MIGRATION_TESTING.md). It selects a
database/mode and an explicitly disposable schema from JSON configuration.

Consumers use the upstream library directly:

```go
import (
    "errors"

    "github.com/golang-migrate/migrate/v4"
    _ "github.com/golang-migrate/migrate/v4/source/file"
    _ "github.com/cocosip/migrate-plus/database/oracle"
)

m, err := migrate.New("file://migrations", databaseURL)
if err != nil {
    return err
}
defer m.Close() // Handle both close errors in the calling application.
if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
    return err
}
```

Use upstream `source/iofs.New(fsys, path)` with
`migrate.NewWithSourceInstance` for embedded migration files. Use the database
package's `WithInstance` function and `migrate.NewWithDatabaseInstance`
or `migrate.NewWithInstance` when reusing a database connection.

Existing database and source drivers remain upstream imports. They do not need
to be copied into migrate-plus. A compiled upstream CLI cannot discover an
additional Go driver at runtime; that driver must be included in a CLI build.

Oracle uses a session lock that survives DDL commits. The migration account
requires `EXECUTE ON SYS.DBMS_LOCK`, `DBMS_UTILITY`, and the privileges/quota
required by its scripts. User SQL is executed as written, without automatic
conversion between Oracle versions. See [Oracle setup, script rules, and tests](database/oracle/README.md).

## Layout

```text
go.mod                 Direct dependency on golang-migrate/v4
cli/                   Upstream-derived command layer calling upstream migrate
database/oracle/       Native Oracle driver, unit tests, and opt-in integration tests
database/dm/           Native DM8 driver, unit tests, and opt-in integration tests
database/gaussdb/      Native GaussDB driver, unit and opt-in integration tests
database/opengauss/    Native openGauss driver, unit and opt-in integration tests
database/oceanbase/    Native OceanBase driver (MySQL and Oracle tenants), unit and opt-in integration tests
cmd/migrate-plus/      Thin executable entrypoint importing all five drivers
docs/                  Architecture and design
.github/workflows/     CI, CodeQL, and tag-based CLI releases
LICENSE                Project MIT license
THIRD_PARTY_NOTICES.md  Derived CLI attribution and license notice
```

The command layer is available as `github.com/cocosip/migrate-plus/cli` for custom
binaries that blank-import their own drivers and call `cli.Main("build-version")`.
It is derived from upstream `internal/cli`, which Go does not allow external
modules to import. Only the command layer is ported; see
[third-party notices](THIRD_PARTY_NOTICES.md).

## Development

CI runs Go commands directly. There is no Makefile.

```powershell
$env:CGO_ENABLED = '0'
go test ./...
go build ./...
go vet ./...
golangci-lint run ./...
go build -trimpath -ldflags '-X main.Version=dev' -o bin/migrate-plus.exe ./cmd/migrate-plus
.\bin\migrate-plus.exe -version
```

GitHub Actions uses Go 1.27.x to test on Linux, Windows, and macOS, verify modules,
run vet, compile integration tests without a database, collect coverage, and
cross-build Linux/macOS/Windows CLI binaries for amd64/arm64. Build artifacts and
each platform's coverage are retained for 14 days. Lint runs the same
golangci-lint v2.14.0 configuration on Linux, Windows, and macOS, matching the
local installation and including integration test sources.
All CI tests and builds disable cgo. CI runs on master/main/develop pushes, pull requests,
and manual dispatch. CodeQL analyzes Go on those branch pushes, `v*` tags, pull
requests, manual dispatch, and a weekly schedule. Dependabot checks Go modules
and Actions weekly.
Default tests require no database. Oracle, DM, GaussDB, openGauss and OceanBase integration tests require an explicitly
configured disposable schema and destroy its supported objects; see the
[driver guide](database/oracle/README.md#integration-tests).

Pushing a `v*` tag reuses the same CI workflow through the release checks job
before publishing a GitHub Release, avoiding a duplicate tag-triggered CI run.
Release notes list commits since the previous tag and link to the full comparison.
Each release includes six `.tar.gz` CLI packages and SHA-256 checksums. Packages
contain the CLI, this project's `LICENSE`, `THIRD_PARTY_NOTICES.md`, and `README.md`;
the binary's `-version` reports the tag. Tags containing `-` create prereleases.

Design documentation: [project architecture](docs/DESIGN.md),
[Oracle driver](docs/ORACLE_DESIGN.md), [DM driver](docs/DM_DESIGN.md),
[GaussDB driver](docs/GAUSSDB_DESIGN.md),
[openGauss driver](docs/OPENGAUSS_DESIGN.md), and
[OceanBase driver](docs/OCEANBASE_DESIGN.md).
Connection examples and operational instructions are in each driver package's README.

## License

[MIT](LICENSE), copyright 2026 cocosip. Provenance and terms for derived CLI
files and native client dependencies are in [third-party notices](THIRD_PARTY_NOTICES.md).
