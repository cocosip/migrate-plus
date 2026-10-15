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
| Version / dirty columns | `NUMBER(19)` / `NUMBER(1)` on every version |
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
go run ./cmd/migrate-plus -database 'oracle://user:password@localhost:1521/service' version
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

## Locks and dropping

The driver reserves one session for its lifetime. `DBMS_LOCK.REQUEST` uses
exclusive mode and `release_on_commit => FALSE`, so DDL cannot release the lock.
Contended locks return upstream `database.ErrLocked` immediately. Closing the
driver releases the lock and closes the session. Lock IDs are hashes of schema
and normalized migration table name; rare collisions cause unnecessary
contention. Processes coordinating the same schema must use the same table name.

Upstream `drop` is destructive: it removes the migration schema's user tables
(including their indexes, constraints and triggers), materialized views, views,
sequences, synonyms, packages, procedures, functions, and types. Materialized views
are dropped before listing remaining tables, including tables retained by
`ON PREBUILT TABLE` views. It does not
remove the database, Oracle user, grants, scheduler jobs, or database links.
Close and reopen the driver after dropping. Oracle DDL cannot roll back a
partially failed drop.

## Integration tests

`go test ./...` runs database-free unit tests and skips integration tests unless
`MIGRATE_PLUS_ORACLE_TEST_URL` is set. SQL-boundary mocks do not prove server SQL
or wire-protocol compatibility.

The integration suite calls upstream `database/testing.Test`, exercises
independent-session locking through DDL and close, and checks upstream
up/dirty/force/down behavior with SQL and PL/SQL migrations. **It destroys the
schema's supported objects. Use an empty, disposable, dedicated test schema.**

```powershell
$env:CGO_ENABLED = '0'
$env:MIGRATE_PLUS_ORACLE_TEST_URL = 'oracle://test_user:password@localhost:1521/service?x-statement-timeout=30000'
go test ./database/oracle -run '^TestIntegration$' -count=1 -v
Remove-Item Env:MIGRATE_PLUS_ORACLE_TEST_URL
```

Run this separately on the Oracle versions and `COMPATIBLE` settings you need
to certify. Record actual server versions and results before claiming them as
verified; unit tests simulate capability selection across 10.2, 11g, 12c, 18c,
19c, 21c, and 23ai.
