# DM driver

Native [golang-migrate](https://github.com/golang-migrate/migrate) database driver
using `gitee.com/chunanyong/dm v1.8.23`. It registers the `dm` URL scheme.

See the [DM driver design](../../docs/DM_DESIGN.md) for implementation decisions
and the [project architecture](../../docs/DESIGN.md) for shared contracts.

## Compatibility

The target is DM8 across multiple builds. Initialization identifies the product
from `V$VERSION` and reads the actual schema, case sensitivity and effective
`BACKSLASH_ESCAPE`. Missing functions, privileges or unsupported settings fail
initialization. No DM server build has been verified yet; unit tests and
cross-builds do not certify native SQL or database behavior.

| Server `COMPATIBLE_MODE` | Target | Real-server verification |
|---|---|---|
| 0 | Native DM | Not run |
| 2 | Oracle compatibility | Not run |
| 4 | MySQL compatibility | Not run |
| 7 | PostgreSQL compatibility | Not run |

All modes use DM-native metadata, SQL and locking. These modes do not implement
the corresponding database completely. The client's separate `compatibleMode`
option accepts 0/1/2 in this client release; it does not select server mode.
The driver changes neither setting and does not rewrite migration dialects or
select migration directories. Write numbered SQL for the actual DM configuration.
DM7 and cluster/failover-wide locking are outside the verified scope.

## Usage

```go
import (
    "github.com/golang-migrate/migrate/v4"
    _ "github.com/golang-migrate/migrate/v4/source/file"
    _ "github.com/cocosip/migrate-plus/database/dm"
)

m, err := migrate.New("file://migrations", "dm://user:password@localhost:5236?schema=APP")
// Handle err, call m.Up(), and handle both errors from m.Close().
```

The distributed CLI includes this driver:

```sh
migrate-plus -path ./migrations -database 'dm://user:password@localhost:5236?schema=APP' up
```

Native connection parameters pass through without URL decoding/re-encoding.
Use the native client's raw credential rules; percent escapes are not decoded
by its DSN parser. `/APP` is also accepted as a schema path. Conflicting path
and query schemas, duplicate options and unknown `x-*` parameters are rejected.
Do not log connection URLs. Actual special-character credential connections
still require real-server verification.

| Migration option | Default | Accepted values |
|---|---|---|
| `x-migrations-table` | `schema_migrations` | ASCII identifier, up to 128 bytes; normalized uppercase |
| `x-statement-timeout` | Disabled | Positive integer milliseconds |

`WithInstance(*sql.DB, *Config)` accepts a dedicated native DM pool. A nil config
selects defaults. Success transfers pool ownership; driver `Close` closes it.
Failure leaves the caller's pool open and discards the reserved physical session.
Do not pass a shared GORM/application pool that must remain usable afterward.

The pool must use `autoCommit=true`, `rwSeparate=0`, `doSwitch=0`,
`driverReconnect=false`, `maxRows=0`, `enRsCache=false` and `escapeProcess=false`.
Row limits can hide corrupt state or Drop objects; result caching can return
stale versions, and escape processing rewrites SQL. URL opening supplies these
explicitly and rejects
conflicts. For `WithInstance` they are caller preconditions: the client exposes
no complete public runtime configuration getter. Do not enable other filters or
global configuration that reconnect, replay or route statements to another
session. One reserved session holds metadata operations, SQL and the lock.

## Database prerequisites

The migration account needs access to `V$VERSION`, `SYS.SYSOBJECTS`,
`SYS.SYSCONS`, `SYS.SYSDEPENDENCIES`, and these native functions:
`CURRENT_SCHID`, `SF_GET_SCHEMA_NAME_BY_ID`, `SF_GET_PARA_VALUE`,
`SF_GET_CASE_SENSITIVE_FLAG`, `SF_GET_SESSION_PARA_VALUE`.
Grant the specific access needed on the deployed build, plus privileges and
quota required by the scripts; the driver does not require a blanket DBA role.

`DBMS_LOCK.REQUEST` and `RELEASE` require execution privileges. If the package
is absent, an administrator can install it using
`SP_CREATE_SYSTEM_PACKAGES(1, 'DBMS_LOCK')`. The driver never installs packages
or grants itself permissions. Locks use zero wait and survive commit, rollback
and DDL; upstream migrate controls lock retry/timeout.

Do not change the current schema, compatibility mode, escaping or routing
settings during a migrator's lifetime, including indirectly through routines.
Close and reopen after configuration changes. Timeout or connection-loss
uncertainty terminates the session; construct a new migrator and inspect the
persisted dirty state. No SQL is replayed. DDL and successful earlier SQL may
already be committed; migrations are not promised atomic rollback.

## SQL scripts

Ordinary SQL uses semicolons outside single/double quotes and comments. Doubled
quote escaping is supported; backslash handling follows the effective session
setting. Native procedural blocks and stored units (including CLASS/CLASS BODY) use a
slash on its own line:

```sql
CREATE TABLE items(id BIGINT PRIMARY KEY);
CREATE OR REPLACE PROCEDURE items_noop AS
BEGIN
    NULL;
END;
/
INSERT INTO items VALUES(1);
```

The final procedural block may end at EOF. Internal and final block semicolons
are preserved. The complete input is framed before execution. Syntax validation
still belongs to DM. Oracle q/nq quotes, MySQL backticks, PostgreSQL dollar
quotes, nested comments, inline WITH routines and client commands such as
`DELIMITER`, `SOURCE`, `COPY`, `PROMPT`, `@` and backslash commands are rejected.
`SET` is limited to transaction/constraint statements. This is native SQL
framing, not DIsql, SQL*Plus, mysql or psql emulation.

## Drop

Upstream `Drop` is destructive and includes the version table. It removes
supported objects in the original migration schema: tables, views, materialized
views, sequences, synonyms, functions, procedures, packages, types, classes and
standalone triggers. Native table cleanup handles dependent indexes/triggers.
Foreign keys in that schema are removed first; materialized views are removed
before re-reading remaining objects to account for backing tables.

External dependencies cause refusal before destructive DDL. Dependency cycles
require explicit cleanup SQL. No `CASCADE`, Oracle `PURGE` or `FORCE` is used.
Catalog/DDL errors stop processing; earlier DDL may have committed. Users,
schemas, databases, grants, jobs, links and global system packages are excluded;
this is not a general schema eraser. Close and reopen after Drop. Native catalog
classification and dependency behavior need verification on every tested build.

## Integration tests

Default tests use mocks and need no server. The following tests **destroy
supported objects in the selected schema**; use a disposable schema exclusively.

```powershell
$env:MIGRATE_PLUS_DM_TEST_URL = 'dm://test_user:password@localhost:5236?schema=TEST_USER'
go test -count=1 -v ./database/dm -run '^TestIntegration$'
```

Tests cover upstream conformance, dirty/force/down, DML durability, lock survival
across transactions/DDL, timeout termination, routines and native Drop ordering.
Set `MIGRATE_PLUS_DM_TEST_MATERIALIZED=1` to also test ordinary/prebuilt
materialized views on builds/accounts supporting them. Set
`MIGRATE_PLUS_DM_OTHER_TEST_URL` to another disposable schema on the same server
with cross-schema SELECT privileges to test external dependency refusal and
sentinel preservation. Missing optional prerequisites are reported as skipped.

Run against an older and a recent DM8 build in each target mode; record exact
build, case, escaping, DDL commit settings and results before claiming tested
compatibility. See [design and source references](../../docs/DM_DESIGN.md).
