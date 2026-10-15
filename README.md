# migrate-plus

Additional pure-Go database drivers for
[golang-migrate/migrate](https://github.com/golang-migrate/migrate), plus a CLI
that composes those drivers with the upstream migration library.

Applications keep using `github.com/golang-migrate/migrate/v4`. A migrate-plus
driver implements the upstream `database.Driver` and registers with the upstream
registry. The migration engine, sources, file parsing, errors, and public library
API come from golang-migrate; they are not implemented again here.

## Current status

Oracle is implemented with `github.com/sijms/go-ora/v2 v2.9.0`. Its target range
follows go-ora's documented Oracle 10.2+ baseline. The driver reads the server
version and `COMPATIBLE` at connection time to select supported capabilities.
Real-server verification is separate from that compatibility target; no server
version has been verified by this repository yet. DM remains planned.

Go 1.26 or newer is required; CI uses Go 1.27.x with `CGO_ENABLED=0`.
The distributed CLI includes Oracle and the upstream file source.

```sh
go run ./cmd/migrate-plus -help
go run ./cmd/migrate-plus -version
go run ./cmd/migrate-plus create -ext sql -dir ./migrations -seq init
go run ./cmd/migrate-plus -path ./migrations -database 'oracle://user:password@localhost:1521/service' up
```

`-version` prints the binary build version. The `version` command queries a
configured database's migration version.

## Library integration

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
cmd/migrate-plus/      Thin executable entrypoint importing Oracle
docs/                  Architecture and design
.github/workflows/     Go 1.27 build, test, and lint jobs
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

GitHub Actions tests on Linux and Windows, cross-builds Linux/macOS/Windows for
amd64/arm64, and uses golangci-lint v2.14.0, matching the local installation.
Default tests require no database. Oracle integration tests require an explicitly
configured disposable schema and destroy its supported objects; see the
[driver guide](database/oracle/README.md#integration-tests).

See [architecture and driver development](docs/DESIGN.md).

## License

[MIT](LICENSE), copyright 2026 cocosip. Attribution and license terms for the
derived CLI files are in [third-party notices](THIRD_PARTY_NOTICES.md).
