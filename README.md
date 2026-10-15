# migrate-plus

Additional pure-Go database drivers for
[golang-migrate/migrate](https://github.com/golang-migrate/migrate), plus a CLI
that composes those drivers with the upstream migration library.

Applications keep using `github.com/golang-migrate/migrate/v4`. A migrate-plus
driver implements the upstream `database.Driver` and registers with the upstream
registry. The migration engine, sources, file parsing, errors, and public library
API come from golang-migrate; they are not implemented again here.

## Current status

M1 provides the module dependency, CLI composition, documentation, and CI.
Go 1.26 or newer and `CGO_ENABLED=0` are required; CI runs Go 1.27.x.
There are **no database drivers or
database connections yet**. Oracle and DM are planned, not available packages.

The CLI uses upstream commands and the native file source. It can print help,
print its build version, and create migration files. Migration commands need a
database driver compiled into the binary; this milestone includes none.

```sh
go run ./cmd/migrate-plus -help
go run ./cmd/migrate-plus -version
go run ./cmd/migrate-plus create -ext sql -dir ./migrations -seq init
```

`-version` prints the binary build version. The `version` command queries a
configured database's migration version.

## Library integration

Once a driver is implemented, its consumers use the upstream library directly.
This is the planned DM usage, **not a runnable M1 example**:

```go
import (
    "errors"

    "github.com/golang-migrate/migrate/v4"
    _ "github.com/golang-migrate/migrate/v4/source/file"
    _ "github.com/cocosip/migrate-plus/database/dm"
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
package's future `WithInstance` function and `migrate.NewWithDatabaseInstance`
or `migrate.NewWithInstance` when reusing a database connection.

Existing database and source drivers remain upstream imports. They do not need
to be copied into migrate-plus. A compiled upstream CLI cannot discover an
additional Go driver at runtime; that driver must be included in a CLI build.

## Layout

```text
go.mod                 Direct dependency on golang-migrate/v4
cli/                   Upstream-derived command layer calling upstream migrate
cmd/migrate-plus/      Thin executable entrypoint and future driver imports
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
No database service or credentials are required for M1.

See [architecture and driver development](docs/DESIGN.md).

## License

[MIT](LICENSE), copyright 2026 cocosip. Attribution and license terms for the
derived CLI files are in [third-party notices](THIRD_PARTY_NOTICES.md).
