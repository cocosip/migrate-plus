# migrate-plus Architecture & Design

> Version: v0.3 (draft for review)
> Status: open decisions listed in §13. All docs, code, and comments in this project are written in English.

## 1. Purpose & Goals

migrate-plus is a superset upgrade of [golang-migrate/migrate](https://github.com/golang-migrate/migrate) (v4):

- **Databases upstream does not support**: Oracle, DM (Dameng), and later possibly KingbaseES, GBase, openGauss, etc. Upstream has no Oracle driver (the relevant PR has been open for years); this is the core value of this project.
- **Pure Go only**: cgo is banned entirely (`CGO_ENABLED=0`). Upstream is constrained by cgo drivers (godror, mattn/go-sqlite3, ...) for cross-compilation and static distribution. This project only accepts pure-Go drivers, so official release artifacts cover every supported database out of the box.
- **Compatible user experience**: migration file naming (`{version}_{title}.up.sql` / `.down.sql`), CLI subcommands (`create/goto/up/down/drop/force/version`), and URL query parameters (`x-migrations-table`, ...) stay consistent with golang-migrate, so existing projects switch at near-zero cost.
- **Modernized interfaces**: context as a first-class citizen, `log/slog` logging, `errors.Is/As` error model. These are long-standing pain points in v4 and the core justification for calling this an upgrade.

### Non-goals

- No schema diffing or migration auto-generation (that is Atlas' territory).
- No ORM integration, no Web UI.
- No Go-source-level compatibility with golang-migrate (interfaces are intentionally reshaped, see §11).

## 2. Ground Rules

| Item | Convention |
|---|---|
| Minimum Go version | **1.25** (`go.mod` + `toolchain go1.25.x`), aligned with upstream master; supported until 2027-02 |
| cgo | Banned. All builds run with `CGO_ENABLED=0`; enforced in CI |
| Dependency admission | Pure-Go third-party dependencies only; cgo-based libraries (godror, mattn/go-sqlite3, ...) are rejected by a CI depguard rule |
| Module layout | Single module, single repo (not multi-module); dependencies are isolated at package level, see §5 |
| Language | Docs, code, comments, and commit messages in English |
| Commit convention | gitmoji + conventional commits: `✨ feat(oracle): add oracle database driver` |
| Lint | `.golangci.yml` (already present, v2 config); CI must be warning-free |
| Versioning | SemVer; v0.x is the development phase (interfaces may change); core interfaces freeze at v1.0 |

Dependency isolation rules (so downstream pulls in only what it uses):

- The root package (core) has **zero third-party dependencies** — standard library only.
- `database/<name>/` may depend only on its own driver library, the root package, and `internal/`.
- `source/<name>/` follows the same rule.

## 3. Module Path & Consumption

Module path: `github.com/cocosip/migrate-plus`, derived from the repository URL (`git@github.com:cocosip/migrate-plus.git`). The root package identifier is `migrateplus` (Go identifiers cannot contain hyphens). First stable release is `v1.0.0`, until then `v0.x`.

Downstream projects consume migrate-plus in four ways, all first-class:

**① As a Go library (recommended)** — blank-import the drivers you need; the rest never enter your dependency graph:

```go
import (
    mp "github.com/cocosip/migrate-plus"
    _ "github.com/cocosip/migrate-plus/database/dm"
    _ "github.com/cocosip/migrate-plus/source/iofs"
)

m, err := mp.New("iofs://migrations", "dm://user:pass@host:5236")
if err != nil { ... }
if err := m.Up(); err != nil && !errors.Is(err, mp.ErrNoChange) { ... }
```

**② As the official CLI binary** — `go install` / GitHub Release / Docker:

```bash
go install github.com/cocosip/migrate-plus/cmd/migrate-plus@latest
migrate-plus -path ./migrations -database "dm://..." up
```

**③ One-shot inside CI**:

```bash
go run -tags "dm" github.com/cocosip/migrate-plus/cmd/migrate-plus@v0.1.0 -database "dm://..." up
```

**④ Compose your own CLI binary** (needed when using out-of-tree drivers in CLI form, see §7.5):

```go
package main

import (
    _ "github.com/someone/migrate-plus-gbase"
    mpcli "github.com/cocosip/migrate-plus/cli"
)

func main() { mpcli.Main("dev") }
```

## 4. Repository Layout

The layout keeps the `source/` + `database/` dual abstraction proven by upstream (lowering porting and cognitive cost); the CLI is split into a reusable public package plus a thin entrypoint:

```
migrate-plus/
├── cli/                      # Public CLI implementation: subcommands, flags, help.
│   ├── commands.go           # create/goto/up/down/drop/force/version (ported from upstream internal/cli)
│   ├── flags.go              # global + per-command flags (stdlib flag, no framework)
│   └── main.go               # Main(version string) — reusable entrypoint for custom binaries
├── cmd/
│   └── migrate-plus/         # Official binary: calls cli.Main + one build-tag file per driver
│       ├── main.go           # cli.Main(Version) — enables all first-party drivers
│       ├── mysql.go          # //go:build mysql
│       ├── oracle.go         # //go:build oracle
│       └── dm.go             # //go:build dm
├── database/                  # Database-side abstraction (extension focus of this project)
│   ├── driver.go              # Driver interface + global registry (Register/Open)
│   ├── errors.go              # Sentinel errors: ErrLocked, ErrDatabaseDirty, ...
│   ├── testing/               # Driver compliance test kit (every driver must pass it)
│   ├── mysql/                 # One package per driver, each with its own init() registration
│   ├── postgres/
│   ├── oracle/                # New: not in upstream
│   └── dm/                    # New: not in upstream
├── source/                    # Migration script source abstraction
│   ├── driver.go              # Driver interface + registry
│   ├── errors.go              # ErrNoNextVersion, ...
│   ├── testing/               # Source compliance test kit
│   ├── iofs/                  # Priority implementation: embed / fs.FS
│   └── file/                  # Priority implementation: local directory
├── internal/                  # Internal helpers, not stable for outsiders
│   └── urlx/                  # URL / query-parameter parsing and normalization
├── docs/
│   ├── DESIGN.md              # This document
│   └── DRIVERS.md             # Maintained driver onboarding guide (§8)
├── migrate.go                 # Core: Migrate type, Up/Down/Steps/Migrate
├── migration.go               # Migration buffering, up/down section parsing
├── errors.go                  # ErrNoChange, ErrChangeVersionUnsupported, ...
├── log.go                     # slog adapter (NoOp by default)
├── go.mod / go.sum
├── Makefile                   # build / lint / test / integration / release
├── .github/workflows/         # CI: lint + unit (always), integration (docker)
└── .golangci.yml              # Already present
```

Source drivers to implement first: `iofs` and `file` only (they cover ~95% of use cases); the rest (http, s3, ...) are ported later on demand.

## 5. Driver Organization

Drivers are **not** "all in one thing". There are three distinct layers:

### 5.1 Core (this repo, one module)

The root package defines the driver contract and owns the runtime: `database.Driver` / `source.Driver` interfaces, the URL-scheme registries, the migration state machine, error sentinels, and the compliance test kits (`database/testing`, `source/testing`). It has zero third-party dependencies.

### 5.2 First-party drivers (in-tree, one package each)

First-party drivers live under `database/<name>/` and `source/<name>/`, **one self-contained package per database**, each registering its own URL scheme. They share the repo's single `go.mod`, but dependencies are isolated at the package level: `go.mod` lists the union, while downstream modules only compile (and record in their own `go.mod`) the packages they actually import. This is the model upstream runs with 20+ drivers; it keeps development simple (one repo, one CI, atomic cross-cutting changes).

If the union `go.mod` ever becomes a problem, any `database/<name>` can be converted into a nested module (a `go.mod` inside the package directory). Import paths do not change, so the split is non-breaking. Deferred until actually needed.

### 5.3 Out-of-tree drivers (external repos, no changes to migrate-plus)

The driver contract is a **public SDK**: the interfaces, `database.Register`, and the compliance kit are all importable. Anyone can implement and version a driver in their own repository:

```
github.com/someone/migrate-plus-gbase/
├── go.mod              // module github.com/someone/migrate-plus-gbase
├── gbase.go            // implements database.Driver; init() calls database.Register("gbase", ...)
└── compliance_test.go  // runs database/testing.Test against a local container
```

Consumers pull it in with a blank import next to the core (library mode, §3①). For CLI mode they compose their own binary via the public `cli` package (§3④): blank-import the external driver, call `cli.Main`. Go has no runtime plugin loading without cgo or heavy RPC machinery, and we reject both — composition at build time is the Go-idiomatic answer, and making `cli/` public (instead of upstream's `internal/cli`) is what makes it practical.

## 6. Core Abstractions

### 6.1 database.Driver (database side)

```go
package database

type Driver interface {
    // Open opens a new connection based on the prototype instance and
    // returns a new Driver instance (same semantics as upstream).
    Open(ctx context.Context, url string) (Driver, error)
    Close(ctx context.Context) error

    // Advisory lock: only one migrate-plus instance may operate on the
    // database at a time.
    Lock(ctx context.Context) error
    Unlock(ctx context.Context) error

    // Run executes one migration (the up/down section, already split).
    Run(ctx context.Context, migration io.Reader) error

    // Version-table access: "single row, current version" model, see §8.2.
    SetVersion(ctx context.Context, version int, dirty bool) error
    Version() (version int, dirty bool, err error)
}

// Optional capability interface: implementing it enables the CLI drop command.
type Purger interface {
    Drop(ctx context.Context) error
}
```

Key difference from upstream v4: **every I/O-bound method carries a `context.Context`** (upstream's `Run(io.Reader)` cannot be cancelled or timed out — a long-standing community pain point). `Version()` deliberately takes no ctx: in upstream practice it is at most one light query and is called frequently; if that proves wrong, `VersionCtx` can be added later.

### 6.2 source.Driver (script source side)

```go
package source

type Driver interface {
    Open(ctx context.Context, url string) (Driver, error)
    Close(ctx context.Context) error

    // Pure in-memory navigation, no I/O, therefore no ctx.
    First() (version int, err error)
    Prev(version uint) (prevVersion int, err error)
    Next(version uint) (nextVersion int, err error)
    Last() (version int, err error)
    Read(ctx context.Context, version int) (io.ReadCloser, error)
}
```

### 6.3 Migrate core

```go
package migrateplus

func New(sourceURL, databaseURL string, opts ...Option) (*Migrate, error)

type Option func(*Migrate)
func WithLogger(l *slog.Logger) Option      // NoOp by default
func WithLockTimeout(d time.Duration) Option

// Semantics match upstream:
//   m.Up() / m.Down() / m.Steps(n) / m.Migrate(uint, Direction)
//   m.Version() / m.GracefulStop()
```

Internally `Migrate` honors ctx at every driver call site and preserves `GracefulStop` semantics (finish the current migration, then stop).

### 6.4 Error model (global sentinels + errors.Is)

| Sentinel | Package | Meaning |
|---|---|---|
| `ErrNoChange` | root | up/down had nothing to apply |
| `ErrLocked` | `database` | another instance holds the lock |
| `ErrDatabaseDirty` | `database` | version table is dirty; manual `force` required |
| `ErrNoNextVersion` | `source` | oldest/newest version reached |

Drivers wrap internal errors with `fmt.Errorf("...: %w", err)` to keep the chain intact. Drivers must **not** define private errors with the same meaning (upstream drivers each define their own types, which defeats `errors.Is` — an explicit thing we fix).

### 6.5 Logging

`log/slog` via `WithLogger`, NoOp by default; the CLI `-verbose` flag swaps in `slog.NewTextHandler(os.Stderr)`. No custom Logger interface.

## 7. Driver Registration & the CLI

### 7.1 Registration mechanism

We keep the mechanism proven by upstream — **URL-scheme registry + init() registration + build-tag trimming** — because it serves both consumption forms (blank import for libraries, compile-time trimming for the CLI):

```go
// database/driver.go
package database

var registry = map[string]Driver{}

func Register(name string, d Driver) { registry[name] = d } // called from init()

func Open(ctx context.Context, url string) (Driver, error) {
    // parse scheme -> registry lookup -> d.Open(ctx, url)
}
```

```go
// database/dm/dm.go
package dm

func init() { database.Register("dm", &DM{}) }
```

### 7.2 Build tags & the default binary

On the CLI side, one tag file per driver (`cmd/migrate-plus/dm.go`):

```go
//go:build dm

package main

import _ "github.com/cocosip/migrate-plus/database/dm"
```

**Deliberate difference from upstream**: upstream compiles only a common subset by default and asks users to assemble via `-tags`; because we are cgo-free, the **official binary enables all pure-Go drivers by default** (the only cost is binary size), leaving `-tags` for advanced users who want to trim. This is the foundation of the "every supported database works out of the box" selling point.

### 7.3 Why migrate-plus ships its own CLI

The upstream `migrate` CLI **cannot** be reused for our drivers — this is a hard constraint, not a preference:

1. **Binary level**: the upstream binary statically links only the upstream drivers registered in its registry. Go has no runtime loading of compiled code without cgo or RPC plugins (both banned here), so oracle/dm drivers cannot be attached to the existing `migrate` binary after the fact.
2. **Source level**: the upstream CLI compiles against upstream's `database.Driver` / `source.Driver` interfaces and `Register` functions. Ours differ by design (ctx parameters, sentinel errors, our own registries), so pointing upstream's CLI at our drivers requires forking it anyway.
3. **Driver availability**: upstream's tag-file mechanism only selects among drivers that exist in the upstream repo; our first-party drivers do not.

Therefore migrate-plus ships its own CLI. The cost is low because upstream is **MIT-licensed** and already factored its CLI into a portable package (`internal/cli` with `main.go` calling `cli.Main(version)`): we **port** that package rather than reinvent, adapting calls to our core API (ctx, slog, sentinels). Ported files retain the upstream MIT copyright notice. Estimated effort: 1–2 days, scheduled in M2.

### 7.4 CLI scope & implementation

Commands and semantics match upstream exactly:

| Command | Behavior |
|---|---|
| `create` | create `{version}_{title}.up.sql`/`.down.sql`; flags `-ext`, `-seq`, `-seq-digits`, `-format` (empty/unix/unixNano/Go layout), `-print` |
| `goto V` | migrate (up or down) to version V |
| `up [N]` | apply N (default: all) pending migrations |
| `down [N]` / `-all` | revert N (prompts interactively when neither N nor `-all` is given) |
| `drop [-f]` | purge the database (requires the driver to implement `Purger`) |
| `force V` | force-set version, clears the dirty flag |
| `version` | print current version and dirty state |

Global flags: `-path`/`-source` (alias), `-database`, `-verbose`, `-lock-timeout`, `-prefetch`, `-allow-dirty`. The exact flag surface is pinned during the M2 port with a golden `--help` output test, so accidental drift is caught.

Implementation decisions:

- **stdlib `flag`, no CLI framework** (no cobra): mirrors upstream help/behavior, keeps the `cli` package dependency-free.
- **Public `cli/` package instead of upstream's `internal/cli`**: this is the enabler for §3④ — external drivers get the full CLI for free by composing a tiny `main` that calls `cli.Main`.
- **Additions over upstream CLI** (each small and additive): `version` also lists the enabled drivers (debugging "which build am I running"); explicit `${VAR}` environment expansion in `-database`/`-path` values; slog-based structured logs behind `-verbose`; Ctrl-C triggers graceful stop of the in-flight migration (falls out of ctx + `GracefulStop`).

### 7.5 Composing custom CLI binaries

For out-of-tree drivers (or trimming to a single database), a ~10-line main yields a fully-featured CLI:

```go
package main

import (
    _ "github.com/someone/migrate-plus-gbase"       // any out-of-tree driver
    _ "github.com/cocosip/migrate-plus/database/dm"  // any first-party driver

    mpcli "github.com/cocosip/migrate-plus/cli"
)

func main() { mpcli.Main("custom-build") }
```

The blank imports register the schemes before `cli.Main` parses flags, so `gbase://...` just works. This recipe is documented in `docs/DRIVERS.md`.

## 8. Adding a New Database Driver

### 8.1 Checklist

1. Create the package `database/<name>/` and pick a **pure-Go** driver library (maintainer's choice; the only hard constraint is: no cgo).
2. Implement `database.Driver` (+ optional `Purger`); register the scheme in `init()`.
3. Implement the version table and locking per the conventions below.
4. Add the tag file `cmd/migrate-plus/<name>.go`.
5. Write and pass the `database/testing` compliance suite (including the docker integration test behind `-tags integration`).
6. Register the driver in the README matrix (with an alpha/beta/stable maturity label).

### 8.2 Version table convention

- Table/schema name configured via the URL parameter `x-migrations-table` (default `schema_migration`); parameter names match upstream.
- Recommended model: **single row holding the current version** — `(version BIGINT, dirty BOOLEAN)` — semantically identical to the upstream postgres/mysql drivers; `force`, dirty flags, and `goto` all build on it.
- Types map per database (e.g. Oracle/DM use `NUMBER(19)` + `CHAR(1)`); the DDL is driver-defined, but the **semantics must pass the compliance suite**.
- `SetVersion` should commit in the same transaction as the migration whenever possible. Databases without transactional DDL (e.g. Oracle, where DDL auto-commits) must acquire the lock first; the lock is what makes crash recovery possible.

### 8.3 Lock convention

Choose by priority and document the capability:

1. Native database advisory locks (MySQL `GET_LOCK`, PG advisory lock, MSSQL `sp_getapplock`, ...).
2. Row lock on the version table (`SELECT ... FOR UPDATE`).
3. A database-internal mutex table when neither exists.

**Silent no-ops are forbidden** (some upstream drivers do this) — a driver without locking must be documented as such and capped at alpha maturity.

### 8.4 Multi-statement & dialects

- One URL parameter, `x-multi-statement=true|false`, controls whether the whole script is sent at once or executed statement by statement; each driver declares and documents its default.
- The statement splitter (handling `--` comments, string literals, `BEGIN...END` blocks) ships as a shared utility in the root package (`migration.SplitStatements`); drivers reuse it instead of writing their own.

### 8.5 Compliance test kit

`database/testing` exposes `Test(t *testing.T, url string, name string)` and asserts uniformly: Open/Close, automatic version-table creation, SetVersion/Version round-trip, dirty flag, lock mutual exclusion (two instances racing for the lock), Run idempotency, and `Purger` when implemented. **Passing the kit is the single merge criterion for a new driver.** The docker-backed parts sit behind `-tags integration` with connection strings injected via `DATABASE_URL_<NAME>` environment variables. The kit is a public package, so out-of-tree drivers run the exact same suite.

## 9. Testing Strategy

| Layer | Content | Dependencies | Trigger |
|---|---|---|---|
| Unit | core state machine, migration parsing, urlx, errors, CLI flag parsing (golden `--help`) | none | every commit |
| Compliance kit | driver behavioral semantics | docker (per-database images) | CI `integration` job |
| CLI end-to-end | real database, up/down/goto/force | docker | CI `integration` job |

## 10. Build & Release

- `Makefile` targets: `build` (all drivers by default), `build-slim` (`-tags` trimming), `lint`, `test`, `integration`, `release`.
- CI gates: `CGO_ENABLED=0 go build ./...` across the platform matrix (linux/darwin/windows × amd64/arm64) + golangci-lint + depguard (deny-list of cgo dependencies).
- Releases via goreleaser: multi-platform binaries + Docker image (`ghcr.io/cocosip/migrate-plus`).
- The CLI command is named `migrate-plus` to avoid clashing with an installed official `migrate`; subcommands and flags keep upstream spelling.

## 11. Compatibility with golang-migrate v4

**Kept compatible**: migration file naming and parsing, CLI subcommand/flag semantics, `x-*` URL parameter names, version-table semantics, `migrate.New(sourceURL, databaseURL)` argument order.

**Intentionally broken** (the upgrade):

| Item | v4 | migrate-plus |
|---|---|---|
| context | no ctx in interfaces | ctx on every I/O method |
| logging | custom Logger interface | `log/slog` |
| errors | per-driver private error types | global sentinels + `errors.Is` |
| driver dependencies | cgo allowed | pure Go only |
| module | `golang-migrate/migrate/v4` | `github.com/cocosip/migrate-plus` |

Porting an upstream driver is mechanical work (add ctx parameters, swap to sentinel errors): estimated 0.5–1 day per driver. Porting the CLI is estimated 1–2 days (§7.3).

## 12. Milestones

| Phase | Scope | Acceptance |
|---|---|---|
| M1 skeleton | go.mod, root package (interfaces/registry/errors/slog/migration parsing), public `cli/` package + `cmd/migrate-plus` entrypoint, Makefile + CI | lint/test green, CLI `version` runs (no drivers yet) |
| M2 end-to-end | port `source/iofs`, `source/file`, one pure-Go database driver (mysql recommended — smallest dependency) + compliance kit + CLI command port | CLI end-to-end up/down/force passes; golden `--help` snapshot |
| M3 first new database | onboard oracle or dm; distill `docs/DRIVERS.md` from the experience | compliance kit + docker integration green |
| M4 release | goreleaser, Docker image, README driver matrix | v0.1.0 |

## 13. Open Decisions

1. **License**: Apache-2.0 or MIT suggested. Ported upstream code (CLI from M2, drivers from M3 on) retains upstream MIT notices — compatible with either choice.
2. **Mandate `database/sql`?** The current design lets drivers sit on any transport (only the interface matters). If every target database has a `database/sql` driver, tightening this to "must be based on database/sql" would simplify the compliance kit.
