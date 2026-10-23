# Migration regression coverage contract

Date: 2026-10-23
Status: Coverage contract for `cmd/migrate-test`. See `MIGRATION_TESTING.md` and
the executable `-list` manifest for implemented assertions and unresolved cases;
this contract does not certify completeness or real-server compatibility.

## Purpose and configuration

This is a repeatable developer regression program inside this repository. After
changing code, an agent or developer selects a configured database/mode and runs
the same complete suite against a real server. Building a distributable executable
is useful, but does not replace comprehensive migration and data-type coverage.

```powershell
go run ./cmd/migrate-test -config targets.local.json -target dm-oracle -report report.json
```

Each target declares `name`, `driver`, expected `mode`, explicit disposable
`schema`, and exactly one of a native connection `dsn` or `dsn_env`. Environment
references keep passwords out of configuration; local files with inline passwords
are also accepted and must be ignored by Git. Optional `other_target` names a
second disposable schema for cross-schema cases. Timeouts have finite defaults
and can be supplied in the configuration. Switching targets changes connection
and fixture selection, not program source. Modes are checked against the server.

The program supports Oracle; DM native/Oracle/MySQL/PG; GaussDB A/PG; openGauss
A/PG; and OceanBase MySQL/Oracle. Distinct fixture profiles are required for all
11 combinations. DM server modes 0/2/4/7 are not client `compatibleMode` values.

## Traceability and result rules

Every row below is decomposed into stable case IDs and explicit per-profile
expectations in the implementation. `-list` exposes IDs, expected applicability
and requirements without a database. A coverage manifest maps every public
migration operation, database.Driver method, fixture/type and documented driver
restriction to case IDs. A local completeness check rejects an unmapped entry,
duplicate ID, missing profile entry or referenced SQL/assertion that does not exist.

Each case/profile entry is required, not applicable with a documented reason, or
conditional with a precise prerequisite (server version, native feature or second
schema). Required/conditional cases never disappear because an execution failed.
Unexpected syntax or permission failures cannot dynamically become not applicable.
Applicability comes from the explicit profile/version contract and read-only
server facts, not trial-and-error fallback SQL.

Completeness applies to the exact configured and detected server product,
version, edition, compatibility mode and installed native feature set. It is not
a claim about all releases or every installable extension. Record this identity
in every report; test installed features supported by that contract. An unknown
version/feature combination has unresolved applicability and remains incomplete
until its manifest is established; do not borrow another version's green result.

Reports distinguish pass, fail, skipped, blocked and not_applicable. A missing
prerequisite skips a conditional case and makes full verification incomplete.
A prior failure blocks dependent cases; it does not erase them. Report configured,
executed and untested profiles separately. Every case counts once at leaf level;
suite/group summaries do not inflate totals. A failed expected-negative assertion
is a failure even when some other error occurred.

Exit 0 means at least one target ran, no applicable case failed/was skipped/blocked,
and cleanup/Close succeeded. Exit 1 means failed or incomplete selected coverage;
exit 2 means invalid configuration or no executable selected target. A missing
connection in a deliberately unselected sample target does not affect the run.
The report always states completeness separately from pass/fail counts.

## Migration engine and source coverage

| Operation | Required cases and observations |
| --- | --- |
| Constructors | New(file URL, database URL), NewWithSourceInstance(iofs, database URL), NewWithDatabaseInstance(file URL, existing database driver), NewWithInstance(iofs, existing database driver); initialization and close ownership |
| Source input | Embedded and temporary file fixtures; sequential and timestamp-sized versions; non-consecutive versions; correct up/down ordering; duplicate/invalid filenames; unknown source; missing target version and unavailable source; missing one direction as an upstream NilMigration; empty/comment-only body |
| Up | Fresh install, upgrade from each intermediate version, all remaining versions, repeated Up = ErrNoChange, reopen existing state, no duplicate seed changes |
| Down | All versions to NilVersion; repeat on empty; down migration failure and recovery; verify schema/data at every intermediate version |
| Steps | +1, +N, -1, -N, 0 = ErrNoChange, exact remaining count, beyond available = ErrShortLimit with correct partial progress |
| Migrate | Forward and backward target; same target = ErrNoChange; target 0 as an actual migration version; absent target with correct upstream error and observed progress |
| Run | Explicit Migration list, empty list = ErrNoChange, nil-body migration, explicit target versions including -1; does not apply source version-selection semantics; dirty-state rejection |
| Version | Empty = ErrNilVersion at engine layer and -1 at driver layer; intermediate, latest, dirty, reopen persistence, timestamp-sized value; no cast/precision loss |
| Force | Existing version, 0, -1, valid version absent from source, less than -1 = ErrInvalidVersion; changes metadata only and clears dirty; data remains unchanged; later migration obeys the forced state |
| Failure/dirty | Invalid SQL, missing object, uniqueness/FK/check/null violation, failed down script and mid-file failure; exact error class plus persisted dirty/version and partial effects; Up/Down/Steps/Migrate/Run reject dirty |
| Recovery | Inspect partial DDL/DML, repair only fixture-owned effects, Force the known state, resume; no blind retry or automatic dirty clearing |
| LockTimeout | Bounded contention through upstream API; driver initialization lock wait is independently bounded; failed contender cannot change history/data; next operation acquires after release |
| Prefetch/buffering | Prefetch 0, 1 and default; script larger than DefaultBufferSize; same order/results; source read error and safe cleanup |
| GracefulStop | Stop before starting and deterministically between migrations; completed version clean, pending migration unapplied, lock released; reopen and resume. Coordinate with observer/gates, not timing guesses |
| Close | Both source/database close errors checked; repeated driver Close; normal, failure, timeout and stop paths; subsequent new connection/migrator works |
| Drop | Real engine Drop and driver Drop, empty and populated schema; dependent objects and local FK cycles; verify inventory/history removed; reopen is required and usable |

The upstream engine defines expected behavior. In particular, source errors and
ErrShortLimit can occur after earlier migrations completed; assert actual upstream
semantics instead of inventing atomicity. Negative Force metadata scenarios are
isolated from normal source progression. Do not use nil Migration pointers outside
the upstream API contract.

## Driver and script coverage

| Area | Required cases |
| --- | --- |
| Open / WithInstance | Both production entry paths per driver; valid native options and custom migration table; missing/invalid URL, wrong product/mode/schema, unsupported settings, malformed option, required capability/privilege errors; failed construction leaves caller-owned pools usable |
| Lock / Unlock | First lock, duplicate lock, release/reacquire; independent physical sessions contend; locks survive DDL and supported commits/rollbacks; Close and failed-session disposal release ownership; schema/table lock scopes follow driver contract |
| SetVersion / Version | -1/0/positive/timestamp-sized versions; clean/dirty, repeated writes, observer durability; invalid values rejected according to driver contract; new and existing history tables |
| Existing history | Every accepted column variant, including DM DIRTY BIT/integer/decimal; preserve existing clean/dirty rows; empty history; reject temporary/wrong shape/null/fractional/out-of-range/duplicate state; never rebuild or silently clear |
| Run / SQL framing | Ordinary DDL/DML, multiple statements, comments, strings containing semicolons, escaped quotes/backslashes, Unicode, quoted identifiers, empty scripts; driver-specific slash lines, q-quotes, dollar-quotes and procedural nesting |
| Procedural SQL | Supported anonymous blocks, procedures/functions, triggers, packages/types and local variables; execute routines and verify their effects, not just creation |
| Rejected SQL | Each driver's documented forbidden client/session/schema/transaction command, COPY framing, malformed literal/comment/envelope and inline routine restrictions; assert preflight rejection before preceding script statements can execute where promised |
| Transactions | DML durability across version writes; successful/failed explicit transactions where allowed; unfinished transaction rejection and rollback cleanup; DDL auto-commit partial state; observer session distinguishes committed from uncommitted values |
| Session invariants | Server encoding/escape/case/SQL-mode and autocommit agreement; forbid or detect schema/identity drift and loss of ownership; never transparently replace a lost locked session |
| Timeout/lost session | Slow-statement deadline, lock wait, close a reserved native session, interrupted worker; verify errors, discarded session, lock release/progress and bounded cleanup. External network disruption/server KILL is conditional on an explicitly configured fault facility |
| Drop object classes | Every supported object class in each driver's Drop catalog; correct dependency ordering and FK cycles; unsupported objects cause preflight rejection without partial deletion when promised |
| Cross-schema safety | other_target must resolve to a different disposable schema in the same server/database/tenant with explicit reference privileges. Create owned reference plus sentinel, verify Drop rejects external dependency before deleting and sentinel survives; remove reference and retry; verify cleanup in both schemas |

Permission-error fixtures cannot revoke the runner's own permissions permanently.
If testing a particular denial requires an additional limited account, declare
that exact optional account in configuration and report incomplete coverage when
absent. No administrator account or grant/revoke operation is implicitly assumed.
Documented driver support and rejection contracts are tested independently of
whether a native server would accept more syntax.

## Data-type and data-migration coverage

Each supported native type has create/insert/select/update assertions and, where
legal, default/nullability/precision/length changes, backfill and down migration.
Use migration SQL literals for the primary workflow so the fixture does not
mistake parameter binding support for migration execution support. Observer
queries must preserve original precision and null distinctions. Binding tests,
if used for independent observations/setup, are labeled separately.

| Family | Required values and migration checks |
| --- | --- |
| Integers | Available small/int/big integer widths; 0, -1, min/max; MySQL unsigned bounds where supported; numbers above 2^53; overflow rejection; widening keeps exact values |
| Exact numeric | NUMBER/DECIMAL/NUMERIC; positive/negative/zero, scale, trailing fractional zeros, large precision; widening/backfill; narrowing/out-of-range failure with correct dirty/partial state; never compare through float64 |
| Approximate numeric | REAL/FLOAT/DOUBLE/BINARY_FLOAT/BINARY_DOUBLE as applicable; negative/zero/fraction/exponent, documented tolerance; NaN/infinity only where explicitly supported |
| Boolean/bit | Native BOOLEAN/BIT or exact dialect equivalent; false/true/null and invalid values; do not invent native BOOLEAN for older Oracle or conflate all BIT columns with bool |
| Text | CHAR/VARCHAR and national/Unicode variants; empty/null with mode-specific semantics, ASCII/Chinese/non-BMP text, quotes, semicolons, backslashes, multiline, boundary lengths and trailing CHAR spaces; widening preserves data |
| Large text | TEXT/CLOB/NCLOB or native equivalents; content beyond SQL/driver buffer thresholds; verify full length and checksum/content, including Unicode; no silent truncation |
| Binary | RAW/VARBINARY/BYTEA and BLOB/large binary; zero bytes, 0x00/0xFF, all-byte payload, null/empty distinction per server, large payload; exact byte/length/hash equality |
| Temporal | DATE/TIME/DATETIME/TIMESTAMP with each supported precision; leap day, before/after epoch, fractional seconds, nulls; DATE time component where native; explicit session-independent comparisons |
| Time zones | Supported timestamp with/local/without time zone and MySQL TIMESTAMP/DATETIME distinctions; fixed offsets and UTC instant versus preserved wall time; verify using controlled session time zones |
| Intervals | Year-month/day-second or native INTERVAL; negative, zero, fractional values and arithmetic/backfill; explicitly not applicable where server has no corresponding type |
| Generated values | Sequence/default, identity/auto_increment/generated columns as supported; old/new inserted rows, update/backfill and sequence continuity; default expressions and check/unique/FK/not-null constraints |
| Structured/native types | JSON/JSONB, XML/XMLTYPE, UUID, arrays, enum/domain/composite, Oracle object/collection, spatial/native extension types where available in the declared server profile; structural equality or server canonicalization, nulls and migration/dependency behavior |

Per-profile type-name catalog (expand aliases into concrete case IDs in code):

| Profile group | Concrete native candidates to inventory and test |
| --- | --- |
| Oracle | NUMBER precision/scale, FLOAT/BINARY_FLOAT/BINARY_DOUBLE, CHAR/VARCHAR2/NCHAR/NVARCHAR2, RAW/LONG RAW/LONG, CLOB/NCLOB/BLOB, BFILE with explicit external-file prerequisites, ROWID/UROWID, DATE, TIMESTAMP and time-zone variants, INTERVAL variants, sequence/identity, object/collection, XMLTYPE and version-dependent native JSON/BOOLEAN |
| DM native/Oracle/MySQL/PG | SMALLINT/INT/BIGINT, DECIMAL/NUMERIC/NUMBER aliases, REAL/DOUBLE, BIT/BOOLEAN availability, CHAR/VARCHAR and mode-specific aliases, BINARY/VARBINARY/BLOB/CLOB, DATE/TIME/TIMESTAMP and available time-zone/interval types; each alias/identity/structured type checked against actual DM mode/version, not inferred from the emulated product |
| GaussDB/openGauss A/PG | SMALLINT/INTEGER/BIGINT, NUMERIC/DECIMAL, REAL/DOUBLE PRECISION, BOOLEAN, CHAR/VARCHAR/TEXT, BYTEA and supported binary aliases, DATE/TIME/TIMESTAMP and time-zone variants, INTERVAL, sequence/identity, JSON/JSONB/XML/UUID/arrays, enum/domain/composite and A-mode Oracle aliases according to version |
| OceanBase MySQL | Signed/unsigned integer widths, DECIMAL, FLOAT/DOUBLE, BOOLEAN alias/BIT, CHAR/VARCHAR/TEXT variants, BINARY/VARBINARY/BLOB variants, DATE/TIME/DATETIME/TIMESTAMP/YEAR, JSON, ENUM/SET, auto_increment/generated columns and supported spatial types |
| OceanBase Oracle | NUMBER, BINARY_FLOAT/DOUBLE, CHAR/VARCHAR2/NCHAR/NVARCHAR2, RAW/CLOB/NCLOB/BLOB, DATE/TIMESTAMP and supported time-zone/INTERVAL variants, sequence/identity and supported Oracle object/collection/structured types |

The native candidates are an inventory obligation, not an assertion that every
listed product/version supports each type. Before declaring a profile complete,
its manifest must enumerate concrete supported types, version-gated types and
explicitly unavailable types. Use server metadata/documentation and real evidence
to settle availability. No generic "other types" entry may hide an uncovered
supported native type; an unresolved candidate blocks a full-coverage claim.

Read back data independently after every relevant version, then down/up again.
Check database metadata for type, length, precision, scale, default and nullability,
plus exact row values. Constraint/default/generated-column tests are independent
of type availability. Null, empty, zero and trailing-space semantics are explicit
per mode; normalization must not conceal a data mismatch. Server encodings that
cannot represent required Unicode values are recorded as a capability restriction,
not silently replaced with ASCII.

## Safety, diagnostics and maintenance

Use an explicitly provided empty disposable schema before migration driver
initialization (Open can create history). Reject system/production GOIMAGE targets.
Retain run ownership, recheck actual identity and foreign objects before Drop,
and clean only owned objects. All cross-schema objects are owned and tracked in
both targets. Tests are sequential for shared targets and must have exclusive
access to their schemas. Failed cleanup blocks reuse and is reported as failure.

Each result includes stable case ID, driver/profile, actual server version/mode,
schema, API operation, fixture, expected and actual observation, native error code
or SQLSTATE where exposed, elapsed time and cleanup status. Type cases also name
the native type, test value category and migration stage. Reports never expose
connection passwords. Preserve original failures; do not loosen assertions or
change expected support just to obtain a green run.

Check in fixtures, coverage manifest, runner tests, sample configuration and usage
documentation. Keep real credentials and generated reports ignored. Code changes
can rerun exactly the same selected target and compare reports; upgrading upstream
migrate or a native client requires reviewing and updating the coverage mapping.
Keep existing unit/driver tests. The standalone runner must actually execute its
listed cases; invoking go test or listing existing test names is not coverage.

Local verification follows all four AGENTS.md commands. Real-server runs require
provided targets and report exact pass/fail/skip/blocked/not-applicable counts,
coverage completeness and server/mode scope separately from unit-test results.
