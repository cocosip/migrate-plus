# OceanBase Driver Design

Status: library-foundation checkpoint, 2026-10-22. The accepted design covers
complete integration; [progress and handoff](OCEANBASE_PROGRESS.md) distinguishes
implemented behavior from the work still required. This is not a release-ready
or real-server compatibility claim.

## Architecture and dependency

Add `database/oceanbase` using `github.com/cocosip/go-oceanbase v0.1.1`.
The package implements and registers upstream `database.Driver`. The application
continues to use golang-migrate/v4 for sources, version transitions, scheduling,
dirty errors and recovery. No root facade, new migration engine, shared parser
framework, or database-specific CLI dispatcher is introduced.

One package and one `oceanbase://` scheme support MySQL, Oracle and native mode
auto-detection. Separate public packages for each mode would duplicate connection
ownership and expose redundant configuration. Wrapping the existing Oracle or
upstream MySQL migration drivers would retain different native-client and
database assumptions. Native metadata selects the mode-specific SQL instead.

## Implemented foundation

| File | Responsibility |
|---|---|
| `config.go` | Migration options, native DSN delegation, basic connection safeguards |
| `oceanbase.go` | Registration, ownership, native metadata, pinned session and errors |
| `lock.go` | Native lock lifecycle and commit/rollback ownership probe |
| `version.go` | Mode-specific state table and transactional history writes |
| `sql.go` | MySQL batch preflight and Oracle framing |
| `drop.go` | Scoped catalogs, dependency preflight and supported object cleanup |
| `*_test.go` | Offline configuration, SQL and sqlmock regression tests |

Mode, schema, version and history table are established on one physical
connection. Native `MetadataServerInfo` exposes the actual mode and
`ob_version()` value. Handshake MySQL compatibility versions are not interpreted
as OceanBase versions. Session validation requires an actual nonempty target and
autocommit. An initial native transaction round trip rejects caller-owned active
transactions before lock-survival probes can commit anything.

Version history follows upstream `NilVersion` and dirty semantics. Existing
history is validated without schema rewriting or dirty clearing. SQL runs as
written in the selected dialect; native multi-statements serve MySQL, while
Oracle uses procedural framing based on this repository's established rules.
The package README documents the deliberately restricted script subset.

## Lock decision and unresolved compatibility

Use MySQL `GET_LOCK`/`RELEASE_LOCK` and Oracle `DBMS_LOCK` exclusive session locks.
Tenant isolation is provided by native lock namespaces. Mode, exact schema and
history table form the hashed lock identity. Oracle uses the documented 30-bit
numeric range. Hash collisions can serialize otherwise independent migrations.

Before state-table DDL, take the lock, commit, verify ownership, roll back and
verify ownership again. MySQL uses `IS_USED_LOCK` with the connection ID;
Oracle `REQUEST` must report already-owned. A failed probe discards the session.
Initialization and uncertain lock errors also discard reserved physical sessions.
There is no automatic downgrade, reconnect or write replay.

Official evidence inspected during implementation:

- [OceanBase V4.3.5 DBMS_LOCK overview](https://github.com/oceanbase/oceanbase-doc/blob/V4.3.5/en-US/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/100.dbms-lock-overview-oracle.md)
- [REQUEST restrictions and return values](https://github.com/oceanbase/oceanbase-doc/blob/V4.3.5/en-US/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/150.request-of-oracle-mode.md)
- [REQUEST, Chinese original](https://github.com/oceanbase/oceanbase-doc/blob/V4.4.2/zh-CN/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/150.request-of-oracle-mode.md)
  and [English V5.0.1](https://github.com/oceanbase/oceanbase-doc/blob/V5.0.1/en-US/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/150.request-of-oracle-mode.md)
- [RELEASE return values](https://github.com/oceanbase/oceanbase-doc/blob/V4.3.5/en-US/700.reference/600.pl-reference/300.pl-oracle/1400.pl-system-package-oracle/9400.dbms-lock-oracle/210.release-oracle.md)
- [OceanBase native MySQL lock implementation](https://github.com/oceanbase/oceanbase/blob/master/src/sql/engine/expr/ob_expr_lock_func.cpp)

The sources are the OceanBase documentation repository (`oceanbase/oceanbase-doc`),
not go-oceanbase, which contains no DBMS_LOCK code. The English V4.3.5 and V4.4.2
REQUEST pages (marked `machine-translation: en`) say `release_on_commit` "can only
be set to TRUE" while their own example passes `FALSE`. The Chinese originals of
those versions say the parameter supports `TRUE` and `FALSE`, default `FALSE`, and
English V5.0.1 was corrected to match. The conflict is therefore a translation
error, not a server limitation. Return values (0 success, 1 timeout, 2 deadlock,
3 parameter error, 4 already held, 5 invalid handle) and RELEASE (0, 3, 4, 5)
match this driver's use. `release_on_commit=TRUE` locks cannot be released with
RELEASE and are transaction-scoped, so they cannot protect DDL migrations. The
implementation requires `FALSE` and still verifies actual behavior at
initialization; Oracle mode is documented as Enterprise Edition only, and the
open-source server tree has no Oracle DBMS_LOCK implementation to inspect. The
MySQL path is session-owned (`OUT_TRANS_LOCK`) in `ob_lock_func_executor.cpp`.
Incompatible versions fail before creating history; incompatible versions fail before creating history. MySQL lock
support also depends on tenant data version and proxy/session capabilities.
Documentation and source inspection are not live evidence. Establish actual
supported server/proxy combinations in the next stage before enabling delivery.

## Destructive operations and error boundaries

Drop performs catalog and dependency checks before removing local foreign keys
and supported objects. It never drops a tenant/user/database, forces cascading
deletion or disables foreign-key checks. Oracle materialized views are rejected
until backing-object behavior has a tested implementation. MySQL events and
unknown objects are rejected. Visibility of external references depends on
catalog privileges; dynamic SQL and arbitrary routine-body dependencies are not
fully represented in these catalogs.

The pinned session is discarded on context/network failure, uncertain commit or
broken session invariants. No migration is replayed after a connection change.
Upstream `database.Error` retains native errors in `OrigErr`. Script preflight
does not sandbox trusted migration SQL or interpret procedural dynamic SQL.

## Remaining delivery work

Add native protocol coverage and explicit disposable-schema integration fixtures
for each mode, resolve any actual server compatibility failures, then add CLI
composition/help coverage and update delivery claims. Required checks are full
repository build, uncached tests, lint and diff checks, followed by existing CI
module and pure-Go platform requirements. The next executor must read
`OCEANBASE_PROGRESS.md` before continuing.
