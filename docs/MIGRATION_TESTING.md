# Real-server migration regression

`cmd/migrate-test` is the repository's standalone regression executable. It calls
the production drivers and upstream migrate engine; it does not invoke `go test`.
It embeds separate SQL fixtures for all 11 database/mode combinations and reads
values and native column metadata through an independent observer connection.

## Configure and run

Create an **empty, dedicated disposable schema** and a non-administrator account
with exclusive access to it. The runner creates and drops objects in that schema.
Do not point it at an application database. GOIMAGE, system schemas, privileged
login options and administrator identities are rejected before driver setup.
Existing root objects also prevent startup. Do not change schema contents while
the runner is active; names are deliberately stable for repeatable diagnosis.

```powershell
go build -o bin/migrate-test.exe ./cmd/migrate-test
Copy-Item examples/migration-targets.example.json targets.local.json
$env:MIGRATE_TEST_DM_ORACLE = 'dm://mp_test:password@localhost:5236?schema=MP_TEST'
./bin/migrate-test.exe -config targets.local.json -target dm-oracle -report migration-report.json
```

`go run ./cmd/migrate-test` accepts the same flags. Without `-target`, every
configured target runs sequentially. Unselected sample targets do not need a
connection. `-list` lists case IDs and unresolved prerequisites without connecting;
combine it with `-config` and `-target` to inspect a selected profile.

Each target has `name`, `driver`, `mode`, `schema`, and exactly one of `dsn_env`
or inline `dsn`. Prefer environment references. Optional `other_target` names a
second configured target for cross-schema dependency tests. It must have the
same product, mode, server/database/tenant, and a different empty disposable
schema. Both schemas need explicit SELECT and REFERENCES privileges for those
cases. The runner never grants privileges or assumes an administrator login.

| Driver | Modes | Native connection grammar |
| --- | --- | --- |
| oracle | oracle | `oracle://user:password@host:1521/service` |
| dm | native, oracle, mysql, pg | `dm://user:password@host:5236?schema=MP_TEST` |
| gaussdb | a, pg | `gaussdb://user:password@host:5432/database?search_path=mp_test` |
| opengauss | a, pg | `opengauss://user:password@host:5432/database?search_path=mp_test` |
| oceanbase | mysql, oracle | `oceanbase://user:password@host:2881/database` |

Use native URL options documented in the respective driver guides. The adapter
rejects migration-only `x-` options, credential overrides and privileged sessions;
it adds its own history table and deadlines. DM server modes 0/2/4/7 are checked
against native/oracle/mysql/pg; they are not client `compatibleMode` numbers.
Gauss A mode also uses that product's own fixtures, rather than Oracle SQL.
The observed schema and mode must match configuration; no automatic mode fallback
is performed. Oracle recyclebin tables count as existing objects; owned Oracle
fixture drops use PURGE instead of purging unrelated recycled objects.

The account needs ordinary object creation/deletion privileges for its profile,
the production driver's lock/catalog privileges, and read access to the native
identity and object catalogs. Oracle uses V$VERSION, SYS_CONTEXT and SESSION_ROLES;
DM also uses V$DATABASE, V$INSTANCE and COMPATIBLE_MODE metadata; Gauss uses its
native pg_catalog and information_schema; OceanBase uses native server metadata,
tenant/cluster catalogs and SHOW GRANTS. Catalog access failures fail preflight;
they never justify destructive fallback. Consult each database driver's README
for SQL restrictions, metadata privileges and DBMS_LOCK requirements. Object
tests may additionally need CREATE TYPE/PROCEDURE/TRIGGER/SYNONYM/MATERIALIZED VIEW
and native package execution privileges on the disposable account.

For DM8, reading active INI parameters also requires `SELECT ANY DICTIONARY`.
The native/Oracle test accounts additionally use `CREATE PACKAGE` for package
fixtures, alongside CREATE SESSION/TABLE/VIEW/PROCEDURE/SEQUENCE/TRIGGER/SYNONYM/
MATERIALIZED VIEW. These privileges can be granted directly without enabling DBA.
Missing privileges remain failures; the runner does not grant them itself.

## Coverage and results

[The coverage contract](MIGRATION_TEST_COVERAGE.md) describes the inventory. The
executable manifest is emitted by `-list`. Cases include all four constructors,
Up/Down/Steps/Migrate/Run/Version/Force/Drop/Close, dirty-state recovery, source
errors, prefetch and graceful stop, history compatibility/corruption, locks,
deadlines, SQL framing, procedures, transactions, constraints and dependencies.
Type cases use actual SQL literals, exact readback, updates, default/backfill
metadata, rollback and reapplication. Large integers/decimals never pass through
float64. Unicode, NULL, empty values, all-byte binary and large Unicode LOB values
have explicit expectations.

The manifest also retains unresolved product/version capabilities: certain DM
aliases and native object classes, version-specific Oracle BOOLEAN/JSON/BFILE,
spatial/extensions, controlled timezone scenarios and special float values.
These entries currently report `skipped` with a reason; they are **not executable
support claims**. External network/server-KILL fault injection has no configured
facility yet and is reported separately as skipped; ordinary closed-session and
statement/worker timeout cases do execute. Missing `other_target` also skips its
case. These gaps make full coverage incomplete even if every executed case passes.
Establish their exact native SQL/prerequisites with the supplied server before
changing a skipped entry into an executable assertion. Never hide a server error
by treating unexpected syntax or permissions as an unsupported feature.

Every leaf result has a stable ID, status, diagnostic and duration; the enclosing
target records actual server version, identity, schema and mode. JSON includes
exact pass/fail/skipped/blocked/not_applicable totals and `complete`. Reports are
saved after each result and target. Passwords are redacted from diagnostics.

| Exit | Meaning |
| --- | --- |
| 0 | At least one executed case; every applicable case and cleanup passed |
| 1 | A failure, missing prerequisite, skipped case or blocked case; incomplete |
| 2 | Invalid arguments/configuration or no executable selection |

Each configured target runs in a child process with a finite total deadline
(default 1800 seconds). Native statements default to 15 seconds. A killed worker
preserves its partial report and blocks missing case IDs. Recovery drops objects
only when the saved report proves an initially empty schema, server identity is
unchanged, and every current root belongs to the fixture allowlist. Interrupted
paired-schema runs require manual inspection of both schemas. Failed cleanup
blocks subsequent cases. Never clear dirty state or delete leftovers blindly.

Local unit tests validate the command, safety checks, native adapters, fixtures
and orchestration. They do not establish server compatibility. After driver or
upstream changes, rerun the same selected disposable target and inspect failures
and incomplete coverage as well as the process exit code. Real server validation
is specific to the supplied server version and selected mode.
