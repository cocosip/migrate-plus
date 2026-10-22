# OceanBase Development Progress and Handoff

Checkpoint date: **2026-10-22 (Asia/Shanghai)**.

The user requested a local end-of-day commit and handoff after the current
implementation stage. Work stops at the **library-foundation checkpoint**.
Complete OceanBase integration, default CLI composition and real-server
acceptance remain open. Do not report OceanBase as release-ready.

## Completed scope

- Added `database/oceanbase` with upstream `database.Driver` registration.
- Pinned `github.com/cocosip/go-oceanbase v0.1.1`; no local `replace`.
- Added native URL delegation, migration options and auto/explicit mode support.
- Added reserved physical-session ownership and failure/discard behavior.
- Added native MySQL/Oracle locking with startup commit/rollback survival checks.
- Added mode-specific version tables, transactional state writes and dirty `-1`.
- Added MySQL batch preflight and Oracle SQL/procedural framing.
- Added scoped Drop with visible-dependency preflight and Oracle ordering.
- Added configuration, framing, ANSI_QUOTES, lock, state, error and Drop tests.
- Documented usage, current constraints, architecture and the next work items.
- Existing drivers and CLI composition are unchanged.

## Current verification

Final full-repository checks on Windows with Go 1.27.2 and golangci-lint 2.14.0:

| Check | Result |
|---|---|
| `go build ./...` | Exit 0 |
| `go test ./... -count=1` (JSON output retained locally) | Exit 0; all 7 packages passed |
| `golangci-lint run ./...` | Exit 0; 0 issues |
| `git diff --check` | Exit 0 |
| `go mod verify` | Exit 0; all modules verified |
| `go mod tidy -diff` | Exit 0; no differences |

Test events: **421 passed, 0 failed, 6 skipped**, counting both tests and
subtests. The OceanBase package contributes **46 passed test/subtest events**.
The six skips are existing opt-in database checks: DM Integration,
LockCompatibility and LockProbeMetadata, plus Oracle, GaussDB and openGauss
Integration. No OceanBase live test was skipped: its integration fixtures have
not been implemented yet.

An independent read-only code review found no additional important/critical
findings after the ANSI_QUOTES and materialized-view safeguards were addressed.
The review itself did not run tests. No live database was contacted and no
destructive integration tests were run. Native protocol-peer coverage, Linux
race testing and cross-platform pure-Go build validation are not included in
this checkpoint's executed checks.

## Decisions and constraints to preserve

1. Continue using the upstream migrate library, registry, interfaces and errors.
   Do not add a migration facade or copy the upstream engine.
2. `WithInstance` owns a dedicated pool only on success; failure discards the
   reserved session without taking ownership of the pool.
3. A physical connection cannot be replaced after acquiring a migration lock.
   Network failures and uncertain commits terminate this driver instance.
4. Oracle requires `DBMS_LOCK.REQUEST(..., FALSE)` to survive DDL commits.
   The English V4.3.5/V4.4.2 REQUEST pages ("only TRUE") are a machine-translation
   error: the Chinese originals of every version, and English V5.0.1, say both
   TRUE and FALSE are supported with default FALSE (see OCEANBASE_DESIGN.md).
   Startup probes still reject transaction-scoped behavior; real server
   behavior is unverified. Do not change to TRUE merely to get a successful call.
5. Existing version/dirty history is never automatically reset or rebuilt.
6. Preflight rejects session/transaction/client commands, executable comments,
   inline Oracle WITH routines and top-level SET statements; inside routine bodies
   only transaction control and system-level SET are rejected. It is not an SQL sandbox
   for dynamic routine SQL.
7. Drop rejects Oracle materialized views until backing-object behavior is
   implemented, and rejects MySQL events and other unknown object types.
   Catalog visibility limits external-dependency detection. No forced cascade.
8. No credentials were copied from other projects or historical acceptance runs.
   Real tests need explicitly selected disposable schemas, never production GOIMAGE.

## Resume here

1. Review `docs/OCEANBASE_DESIGN.md` and `database/oceanbase/README.md` against
   the actual source. The ignored implementation scratch plan is optional;
   this committed handoff is the authoritative continuation entrypoint.
2. Add protocol tests through the real go-oceanbase connector. Focus on native
   metadata requests, Oracle OUT binds, transaction-state restoration, negotiated
   multi-statements, cancellation and physical-session loss. Current sqlmock
   tests do not prove these wire-level paths.
3. Opt-in entrypoints exist (`TestIntegrationMySQL/Oracle`, env
   `MIGRATE_PLUS_OCEANBASE_{MYSQL,ORACLE}_TEST_URL`) but have never been run. Run them against explicitly
   disposable MySQL/Oracle schemas. First reproduce the native lock operations,
   cross-session contention and commit/rollback/DDL survival on both targets.
   Record real server version, mode, direct/proxy path and package privileges.
4. Run upstream `database/testing.Test`, repeated Up, failure/dirty/Force/Down,
   reopen-existing-history, canceled/lost-session and dependency-safe Drop
   scenarios. Preserve failures until driver, server or privilege causes are
   established. Check catalog columns/visibility, legacy history types and
   identifier case behavior against actual OceanBase.
5. Refine any restrictive script/Drop behavior only when the requested scope
   needs it and the matching regression/integration evidence exists. Validate
   inline WITH routines and materialized-view backing tables before claiming
   support; neither has dedicated acceptance coverage at this checkpoint.
6. Done: the CLI imports the driver and registration/help tests cover it.
7. Update root architecture/status/release text when delivery scope changes,
   then run all completion checks and the existing pure-Go cross-platform CI
   requirements. Commit locally; push only if explicitly requested.

## Reproducible local commands

Run from the repository root in PowerShell:

```powershell
$env:GOCACHE = Join-Path (Get-Location).Path '.cache/go-build'
$env:GOMODCACHE = Join-Path (Get-Location).Path '.cache/go-mod'
$env:GOLANGCI_LINT_CACHE = Join-Path (Get-Location).Path '.cache/golangci-lint'
$env:GOPATH = Join-Path (Get-Location).Path '.cache/go-path'

go build ./...
go test ./... -count=1
golangci-lint run ./...
git diff --check
```

The extra local GOPATH avoids Windows access denial when Go's checksum database
cache otherwise uses the default user GOPATH. Module checksum verification stays
enabled. Do not work around this by disabling GOSUMDB.

Module checks: `go mod verify` and `go mod tidy -diff`. Existing CI also requires
CGO_ENABLED=0 builds for linux/darwin/windows on amd64/arm64 and a separate Linux
race job. A local Windows build does not substitute for those environments.

Use `git log -1 -- docs/OCEANBASE_PROGRESS.md` to identify this checkpoint commit.
This handoff intentionally contains no database connection secrets.
