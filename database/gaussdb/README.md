# GaussDB driver

Native Huawei GaussDB driver for `github.com/golang-migrate/migrate/v4`, using
the pure-Go [Huawei client](https://github.com/huaweicloud-samples/database-gaussdb-go)
at `github.com/HuaweiCloudDeveloper/gaussdb-go v1.0.0-rc1` (a prerelease).
The client's documented server baseline is GaussDB 2.x+. This is a target,
not a real-server certification: no Huawei GaussDB build has been exercised
by this repository yet. See the [design and evidence](../../docs/GAUSSDB_DESIGN.md).

The initial implementation accepts native A and PG compatibility modes and a
writable UTF8 client session without distributed coordinators. It rejects other modes and
coordinator topologies because their SQL framing and cluster-wide lock scope
have not been established. GaussDB(for MySQL), PostgreSQL and openGauss are
not additional supported products.

## URL and CLI

```sh
migrate-plus -path ./migrations -database 'gaussdb://user:password@localhost:5432/app?search_path=app&sslmode=verify-full&x-statement-timeout=30000' up
```

The database path and `search_path` select different things: the path identifies
the database; `current_schema()` resolves the migration schema. Create a
dedicated schema first. Native TLS/certificate, connection-timeout, service,
application-name and multi-host settings follow the client. URL-encode passwords
and query values. The example assumes the client's certificate setup.

| Migration parameter | Meaning |
|---|---|
| `x-migrations-table` | Default `schema_migrations`; ASCII `[A-Za-z][A-Za-z0-9_]*`, normalized to lowercase, at most 63 bytes and within the server identifier limit |
| `x-statement-timeout` | Positive integer milliseconds; bounds the entire SQL file, each internal operation and the entire lock attempt; absent means no driver timeout |

Migration options are removed before native parsing. Unknown `x-*`, duplicate
query parameters, empty migration values, fragments, conflicting database
aliases and read-only routing are rejected. Native environment/service defaults
still apply; initialization also checks the connected session is writable.
`statement_timeout` is a separate native server parameter.

The distributed CLI compiles in this driver. A custom CLI or application must
import it explicitly; Go drivers are selected at build time.

## Library

```go
import (
    "github.com/golang-migrate/migrate/v4"
    _ "github.com/golang-migrate/migrate/v4/source/file"
    _ "github.com/cocosip/migrate-plus/database/gaussdb"
)

m, err := migrate.New("file://migrations", databaseURL)
// Check err, call m.Up()/Down(), and handle both errors returned by m.Close().
```

For an existing client pool, use the native adapter explicitly:

```go
import (
    "time"

    gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"
    "github.com/HuaweiCloudDeveloper/gaussdb-go/stdlib"
    "github.com/cocosip/migrate-plus/database/gaussdb"
    "github.com/golang-migrate/migrate/v4/database"
)

func openDriver(nativeURL string) (database.Driver, error) {
    native, err := gaussdbgo.ParseConfig(nativeURL)
    if err != nil {
        return nil, err
    }
    pool := stdlib.OpenDB(*native)
    driver, err := gaussdb.WithInstance(pool, &gaussdb.Config{
        StatementTimeout: 30 * time.Second,
    })
    if err != nil {
        _ = pool.Close()
        return nil, err
    }
    return driver, nil
}
```

Use upstream `migrate.NewWithDatabaseInstance` with the returned driver, or
`migrate.NewWithInstance` with an embedded `source/iofs` instance. Do not expose
the native client's credential-bearing parse errors to untrusted users.

`WithInstance` accepts only Huawei's native `stdlib` adapter. It reserves one
physical connection and copies Config (nil selects defaults). Success transfers
ownership of the pool: driver Close closes the session and pool. Use a dedicated
pool. Failed construction discards its reserved connection but leaves the
caller-owned pool open. Config timeout zero means no timeout; negative is invalid.

## SQL files and state

Run sends the entire file unchanged through native simple-query execution.
It does not split on semicolons, translate dialects, or insert transaction
commands. For example:

```sql
CREATE TABLE app.items(id BIGINT PRIMARY KEY, label VARCHAR(100));
INSERT INTO app.items VALUES (1, 'first');
CREATE FUNCTION app.echo(integer) RETURNS integer
AS $$ SELECT $1 $$ LANGUAGE SQL;
```

Native quoted/dollar-quoted routine bodies are accepted. GaussDB must support
the particular routine syntax in the selected mode. Oracle/DM unquoted
procedural blocks and standalone `/` terminators are rejected. gsql/psql
backslash commands, `DELIMITER`, `SOURCE`, `SPOOL` and `PROMPT` are unsupported.
All top-level COPY commands, including server-file COPY, are rejected: the
selected simple-query client cannot safely provide a COPY stream.

SQL files must contain valid UTF8. The limited lexical preflight uses the session's
actual compatibility, `standard_conforming_strings` and `client_encoding` settings,
refreshed before each nonempty file; non-UTF8 client encodings are rejected.
It recognizes nested comments, quoted identifiers, ordinary/escape/dollar
strings and A-mode alternative quotes. It is not a complete SQL validator.
Configure lexical settings before migration; scripts must not change their
meaning while a file is being parsed. Avoid changing authentication, routing,
database or lock ownership, including `DISCARD ALL` and advisory-unlock calls.

The driver binds state and lock identity to the original database, schema and
table. User changes to `search_path` do not move the state table. Each file must
leave native transaction status idle. Unfinished/failed transactions return an
error and receive ROLLBACK on the same session with a fresh five-second cleanup
timeout. Failed cleanup, lost connections, operation timeout, or uncertain
commit/rollback discards the physical session. Continuing requires a new driver;
there is no reconnect or SQL replay.

Upstream controls dirty/clean transitions. State uses BIGINT `version` and
integer `dirty` (0/1), committed with transactional DELETE/INSERT, including
dirty version -1. Empty history returns upstream NilVersion. A preexisting
BOOLEAN dirty column is incompatible; invalid or multiple state rows fail.
The state table must be an ordinary table with a non-null BIGINT primary-key
version and non-null integer dirty column. It is not converted automatically.

Do not assume atomic DDL across modes/builds. Earlier statements may already
have committed when a file fails. Repair actual database state before upstream
`force`; force does not undo SQL.

## Locking, privileges and Drop

Lock uses native session `pg_try_advisory_lock(bigint)` and
`pg_advisory_unlock(bigint)`, retrying contention every 100 ms. The lock must
survive commit/rollback/DDL. Upstream LockTimeout only limits the caller's wait;
Close cancels pending acquisition. Use the driver timeout to bound acquisition
itself. Initialization acquires the same lock around state-table setup.

The account needs CONNECT, schema USAGE/CREATE, access to native catalogs and
settings, EXECUTE on the advisory functions, DML on the state table, and the
permissions required by its migration SQL. Drop additionally needs ownership
or equivalent privileges for every object it removes. Do not target system
schemas or share migration pools with application traffic.

Drop removes supported tables, views/materialized views, sequences, native
functions/procedures and user-defined types/domains in the original schema,
including the state table. It preflights catalog dependencies and rejects
external dependents, external owned objects, extension ownership, unsupported
objects and unresolved cycles before DDL. Local foreign keys are removed first;
other objects are ordered by dependency. Native RESTRICT is used without
CASCADE. Concurrent catalog changes or a later permission/DDL error can leave
partial cleanup. Close and reopen after Drop. Never run it on a shared schema.

## Integration tests

Normal tests use SQL mocks and the real native adapter over an in-memory wire;
they need no database. Real-server tests are opt-in and destructive:

```powershell
$env:MIGRATE_PLUS_GAUSSDB_TEST_URL = 'gaussdb://user:password@host:5432/testdb?search_path=disposable&sslmode=verify-full&x-statement-timeout=15000'
go test ./database/gaussdb -run TestIntegration -count=1 -v
```

Use an empty disposable schema. The suite reuses upstream conformance tests and
checks lock survival through DDL/commit/rollback, version durability, dirty
force/down recovery, transaction cleanup, timeout disposal and dependency Drop.
Set `MIGRATE_PLUS_GAUSSDB_TEST_OTHER_URL` to a second disposable schema in the
same database for cross-schema dependency checks; the account needs sufficient
privileges in both schemas. Tests must not run concurrently on those schemas.

Record exact server build, mode and topology when running these tests. They
have not been run against Huawei GaussDB here. Distributed locking, failover
and other compatibility modes remain unverified and are not enabled.
