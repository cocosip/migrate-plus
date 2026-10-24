# Oracle

Native [golang-migrate](https://github.com/golang-migrate/migrate) database driver
using [`github.com/sijms/go-ora/v2 v2.9.0`](https://github.com/sijms/go-ora/tree/v2.9.0).
No cgo, Oracle client installation, or replacement migration engine is required.

See the [Oracle driver design](../../docs/ORACLE_DESIGN.md) for implementation
decisions and the [project architecture](../../docs/DESIGN.md) for shared contracts.

## Compatibility

The target baseline is Oracle **10.2+**, as documented by go-ora v2.9.0.
The driver reads server version and `COMPATIBLE` once using
`DBMS_UTILITY.DB_VERSION`; it does not query privileged `V$` views. Both must
be at least 10.2. Oracle 10.1 and older are rejected.

| Capability | Selection |
|---|---|
| Migration table name limit | 30 bytes; 128 when both server and `COMPATIBLE` are at least 12.2 |
| Version / dirty columns | `NUMBER(19)` / `NUMBER(1)` on every version; existing tables must be permanent (`TEMPORARY='N'`, no `DURATION`) and keep `NUMBER` columns with at least that precision |
| Locking | `DBMS_LOCK` session lock on every version |
| User migration SQL | Executed without rewriting; the author targets the server's SQL dialect |

Shared SQL stays shared. New version branches belong at actual capability
differences, not in separate driver implementations for every Oracle release.
Target compatibility does not imply testing on every version. **No Oracle server
version has been verified by this repository yet.**

## Database account

Use a dedicated migration account with `CREATE SESSION`, a tablespace quota,
and privileges needed by its scripts, such as `CREATE TABLE`, `CREATE SEQUENCE`,
and `CREATE PROCEDURE`. The integration suite also needs `CREATE MATERIALIZED VIEW`.
An administrator must grant:

```sql
GRANT EXECUTE ON SYS.DBMS_LOCK TO migration_user;
```

`DBMS_UTILITY` must also be executable; it is normally available through PUBLIC.
Grant it explicitly if your installation restricts it. Do not run migrations as
SYS or SYSTEM. Objects and migration state belong to the schema selected when
the driver opens. Version queries always qualify that original schema.

## URL and CLI

```text
oracle://user:password@host:1521/service?x-migrations-table=schema_migrations&x-statement-timeout=30000
```

| Migration option | Default | Meaning |
|---|---|---|
| `x-migrations-table` | `schema_migrations` | ASCII identifier beginning with a letter, followed by letters, digits, `_`, `$`, or `#`; normalized to uppercase |
| `x-statement-timeout` | No timeout | Positive milliseconds per database operation, including initialization and lock calls |

Quoted or schema-qualified table overrides are not accepted. Unknown or duplicate
migration options are rejected. Native go-ora connection options pass through
unchanged; `SID`, wallets, TLS, and connection timeouts use go-ora's documented
configuration. Percent-encode URL credentials, or build the URL with
`go_ora.BuildUrl`. Connection failures are returned to the caller.

```sh
go run ./cmd/migrate-plus -path ./migrations -database 'oracle://user:password@localhost:1521/service' up
go run ./cmd/migrate-plus -path ./migrations -database 'oracle://user:password@localhost:1521/service' down 1
go run ./cmd/migrate-plus -path ./migrations -database 'oracle://user:password@localhost:1521/service' version
```

## Package integration

Use upstream migrate normally, with this driver registered:

```go
import (
    "github.com/golang-migrate/migrate/v4"
    _ "github.com/golang-migrate/migrate/v4/source/file"
    _ "github.com/cocosip/migrate-plus/database/oracle"
)

m, err := migrate.New("file://migrations", databaseURL)
```

For an existing go-ora `*sql.DB`, call
`oracle.WithInstance(db, &oracle.Config{MigrationsTable: "schema_migrations"})`,
then `migrate.NewWithDatabaseInstance(sourceURL, "oracle", driver)`. A nil config
uses the defaults. Configuration is copied rather than modified.

Successful `WithInstance` transfers pool ownership: driver `Close` and upstream
`Migrate.Close` close the reserved connection **and the entire pool**. Supply a
dedicated pool rather than an application pool still in use. On construction
failure the pool remains caller-owned; the reserved session is discarded to
avoid returning a possible session lock to the pool. Close the driver if later
migrator construction fails, and handle both errors returned by `Migrate.Close`.

## SQL scripts

Ordinary SQL statements end at a semicolon outside comments, quoted identifiers,
strings, and Oracle q-quotes. The semicolon is removed before execution.
Anonymous PL/SQL blocks, procedures, functions, packages, triggers, and types
retain internal semicolons; separate these units with `/` on its own line.
A final unit can end at EOF without a slash. SQL containing inline
`WITH FUNCTION` / `WITH PROCEDURE` declarations follows the same slash boundary
rule, while its final SQL semicolon is removed. Put a slash before any following
statement so it is not combined with the inline routine's query.

```sql
CREATE TABLE example_items (id NUMBER PRIMARY KEY, name VARCHAR2(100));
CREATE SEQUENCE example_items_seq;

CREATE OR REPLACE TRIGGER example_items_bi
BEFORE INSERT ON example_items
FOR EACH ROW
BEGIN
    IF :NEW.id IS NULL THEN
        :NEW.id := example_items_seq.NEXTVAL;
    END IF;
END;
/

INSERT INTO example_items (name) VALUES ('first; item');
```

This sequence/trigger example works with the older SQL baseline. An author
targeting a newer Oracle can use that version's identity syntax instead. The
driver does not translate identity columns, types, or business DDL. SQL*Plus
commands such as `SET DEFINE`, `SPOOL`, `@file`, and substitution variables are
not interpreted. SQL bind variables without values are not migration parameters.

Oracle DDL commits implicitly. A failed migration can leave earlier statements
applied, while upstream migrate retains the dirty version. Repair the schema
before using upstream `force` to record the intended version. The driver does
not silently retry migration SQL after a failure.

Empty history reports version -1. Initialization and version reads reject
multiple state rows, versions below -1, and dirty values other than 0/1; repair
corrupt history explicitly before reopening the driver.

An execution timeout or a failed version-transaction commit/rollback discards
the session. Construct a new migrator and inspect the persisted schema and dirty
state before continuing; a failed commit may already have reached the server.

## Locks and dropping

The driver reserves one session for its lifetime. `DBMS_LOCK.REQUEST` uses
exclusive mode and `release_on_commit => FALSE`, so DDL cannot release the lock.
The driver retries contention every 100 ms. `Migrate.LockTimeout` (CLI
`-lock-timeout`) bounds the caller's wait; upstream does not retry or cancel
`Driver.Lock`. Always call `Close` after a lock timeout to cancel pending
acquisition and release any acquired lock. `x-statement-timeout` bounds the
whole driver lock attempt, including retries. Initialization additionally uses
`migrate.DefaultLockTimeout` (15 seconds); the CLI sets it from `-lock-timeout`
before opening the driver. A shorter statement timeout takes precedence. Library
callers must configure the default before construction, rather than changing
an instance's `LockTimeout` afterward. Repeated
`Lock` calls on the same driver return `database.ErrLocked`.

Closing the driver releases the lock and closes the session. Lock IDs are
hashes of schema and normalized migration table name; rare collisions cause unnecessary
contention. Processes coordinating the same schema must use the same table name.

Upstream `drop` is destructive: it removes the migration schema's user tables
(including their indexes, constraints and triggers), standalone triggers such as
schema event triggers, materialized views, views,
sequences, synonyms, packages, procedures, functions, and types. Standalone
triggers are dropped first and system-generated triggers are left alone;
materialized views
are dropped before listing remaining tables, including tables retained by
`ON PREBUILT TABLE` views. It does not
remove the database, Oracle user, grants, scheduler jobs, or database links.
Before DDL it checks visible external dependencies and type cycles, then removes
only this schema's foreign keys. Types are dropped in dependency order.
Table drops use `PURGE` without `CASCADE CONSTRAINTS`; types never use `FORCE`,
so native dependency checks protect external constraints even when catalog
visibility is incomplete. Catalog visibility depends on the migration account;
preflight cannot discover every external dependent under restricted privileges.
Close and reopen the driver after dropping. Oracle DDL cannot roll back a
partially failed drop.

## Integration tests

`go test ./...` runs database-free unit tests; integration tests compile only
behind the `integration` build tag and additionally require
`MIGRATE_PLUS_ORACLE_TEST_URL` to be set. SQL-boundary mocks do not prove server SQL
or wire-protocol compatibility.

The integration suite calls upstream `database/testing.Test`, exercises
independent-session locking through DDL and close, and checks upstream
up/dirty/force/down behavior with SQL and PL/SQL migrations. Local foreign-key
cycles and dependent types are included. Set `MIGRATE_PLUS_ORACLE_OTHER_TEST_URL`
to a second disposable schema on the same server to check that an external
foreign key survives a refused Drop; both accounts need table-creation rights,
and the parent account must be able to grant REFERENCES. **It destroys the
schema's supported objects. Use an empty, disposable, dedicated test schema.**

```powershell
$env:CGO_ENABLED = '0'
$env:MIGRATE_PLUS_ORACLE_TEST_URL = 'oracle://test_user:password@localhost:1521/service?x-statement-timeout=30000'
go test -tags integration ./database/oracle -run '^TestIntegration$' -count=1 -v
Remove-Item Env:MIGRATE_PLUS_ORACLE_TEST_URL
```

Run this separately on the Oracle versions and `COMPATIBLE` settings you need
to certify. Record actual server versions and results before claiming them as
verified; unit tests simulate capability selection across 10.2, 11g, 12c, 18c,
19c, 21c, and 23ai.
