# openGauss driver

Native openGauss driver for `github.com/golang-migrate/migrate/v4`, using the
official pure-Go client
[`gitcode.com/opengauss/openGauss-connector-go-pq v1.0.9`](https://gitcode.com/opengauss/openGauss-connector-go-pq)
(a `lib/pq` fork with openGauss SHA256/SM3 password authentication). The
project historically published that client under `gitee.com/opengauss/...`;
releases from 2025 declare the gitcode.com module path used here. This is a
target, not a real-server certification: no openGauss build has been exercised
by this repository yet. See the [design and evidence](../../docs/OPENGAUSS_DESIGN.md).

The initial implementation accepts native A and PG compatibility modes and a
writable UTF8 client session without distributed coordinators. It rejects other
modes and coordinator topologies because their SQL framing and cluster-wide
lock scope have not been established. B/C compatibility modes, plain PostgreSQL
and Huawei GaussDB are outside this driver's scope: GaussDB has its own
[GaussDB driver](../gaussdb/README.md), and PostgreSQL is served upstream.

## URL and CLI

```sh
migrate-plus -path ./migrations -database 'opengauss://user:password@localhost:5432/app?sslmode=disable&search_path=app&x-statement-timeout=30000' up
```

The database path and `search_path` select different things: the path identifies
the database; `current_schema()` resolves the migration schema. Create a
dedicated schema first. Native TLS/`sslmode`, certificate, connection-timeout,
service and application-name settings follow the client. URL-encode passwords
and query values. The client also accepts comma-separated primary/standby
`host:port` pairs in the authority and selects a writable primary.

| Migration parameter | Meaning |
|---|---|
| `x-migrations-table` | Default `schema_migrations`; ASCII `[A-Za-z][A-Za-z0-9_]*`, normalized to lowercase, at most 63 bytes and within the server identifier limit |
| `x-statement-timeout` | Positive integer milliseconds; bounds the entire SQL file, each internal operation and the entire lock attempt; absent means no driver timeout |

Migration options are removed before native parsing. Unknown `x-*`, duplicate
query parameters, empty migration values, fragments, conflicting database
aliases and read-only routing are rejected. Initialization verifies the server
banner identifies openGauss, the session is writable, and the compatibility
mode is A or PG.

The distributed CLI compiles in this driver. A custom CLI or application must
import it explicitly; Go drivers are selected at build time.

## Library

```go
import (
    "github.com/golang-migrate/migrate/v4"
    _ "github.com/golang-migrate/migrate/v4/source/file"
    _ "github.com/cocosip/migrate-plus/database/opengauss"
)

m, err := migrate.New("file://migrations", databaseURL)
// Check err, call m.Up()/Down(), and handle both errors returned by m.Close().
```

For an existing client pool, use the client's connector explicitly:

```go
import (
    "database/sql"
    "time"

    opq "gitcode.com/opengauss/openGauss-connector-go-pq"
    "github.com/cocosip/migrate-plus/database/opengauss"
    "github.com/golang-migrate/migrate/v4/database"
)

func openDriver(nativeURL string) (database.Driver, error) {
    connector, err := opq.NewConnector(nativeURL)
    if err != nil {
        return nil, err
    }
    pool := sql.OpenDB(connector)
    driver, err := opengauss.WithInstance(pool, &opengauss.Config{
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

`WithInstance` accepts any `*sql.DB` whose connections speak the openGauss wire
protocol, including pools built from multi-host DSNs. Connection-class failures
(SQLSTATE class `08`) are recognized from any such client through its
`SQLState()` method; FATAL/PANIC severity is recognized from the official
client only. It reserves one physical
connection and copies Config (nil selects defaults). Success transfers ownership
of the pool: driver Close closes the session and pool. Failed construction
discards its reserved connection but leaves the caller-owned pool open. Config
timeout zero means no timeout; negative is invalid.

## SQL files and state

Run sends the entire file unchanged through native simple-query execution.
It does not split on semicolons, translate dialects, or insert transaction
commands into the file. For example:

```sql
CREATE TABLE app.items(id BIGINT PRIMARY KEY, label VARCHAR(100));
INSERT INTO app.items VALUES (1, 'first');
CREATE FUNCTION app.echo(integer) RETURNS integer
AS $$ SELECT $1 $$ LANGUAGE SQL;
```

Native quoted/dollar-quoted routine bodies are accepted. openGauss must support
the particular routine syntax in the selected mode. Oracle/DM unquoted
procedural blocks and standalone `/` terminators are rejected. gsql/psql
backslash commands, `DELIMITER`, `SOURCE`, `SPOOL` and `PROMPT` are unsupported.
All top-level COPY commands, including server-file COPY, are rejected: the
selected simple-query client cannot safely provide a COPY stream.

SQL files must contain valid UTF8. The limited lexical preflight uses the session's
actual compatibility, `standard_conforming_strings` and `client_encoding` settings,
refreshed before each nonempty file; non-UTF8 client encodings are rejected.
It recognizes nested comments, quoted identifiers, ordinary/escape/dollar
strings (including escape-string continuation across newlines and line comments)
and A-mode alternative quotes. It is not a complete SQL validator.
Configure lexical settings before migration; scripts must not change their
meaning while a file is being parsed. Avoid changing authentication, routing,
database or lock ownership, including `DISCARD ALL` and advisory-unlock calls.

The driver binds state and lock identity to the original database, schema and
table. User changes to `search_path` do not move the state table. The client
exposes no transaction-status API. After successful execution, Run probes with
`SAVEPOINT`: success means the file left a transaction open, while SQLSTATE
`25P01` means no transaction is active. Other probe errors fail the migration.
When the session is not confirmed idle — a failed script, an open transaction
or an uncertain probe — Run issues `ROLLBACK`, clearing any open or aborted
transaction; a confirmed idle session needs no rollback. Cleanup has a fresh
five-second timeout. Unfinished transactions return an error and
upstream retains dirty state; their uncommitted rows never commit. Explicit
`BEGIN ... COMMIT` pairs remain supported. Idle probes can log an expected
`25P01` error. A failed cleanup discards the
physical session. Failed connections, operation timeout, or uncertain
commit/rollback also discard the physical session. Continuing requires a new
driver; there is no reconnect or SQL replay.

Upstream controls dirty/clean transitions. State uses BIGINT `version` and
integer `dirty` (0/1), committed with transactional DELETE/INSERT, including
dirty version -1. Empty history returns upstream NilVersion. A preexisting
BOOLEAN dirty column is incompatible; invalid or multiple state rows fail.
The state table must be a permanent ordinary table (`relpersistence='p'`) with
a non-null BIGINT primary-key version and non-null integer dirty column.
Unlogged and temporary tables are rejected. It is not converted automatically.

Do not assume atomic DDL across modes/builds. Earlier statements may already
have committed when a file fails. Repair actual database state before upstream
`force`; force does not undo SQL.

## Locking, privileges and Drop

Lock uses native session `pg_try_advisory_lock(bigint)` and
`pg_advisory_unlock(bigint)`, retrying contention every 100 ms. The lock must
survive commit/rollback/DDL. Upstream LockTimeout only limits the caller's wait;
Close cancels pending acquisition. Use the driver timeout to bound acquisition
itself. Initialization acquires the same lock around state-table setup.
Its lock wait is additionally bounded by `migrate.DefaultLockTimeout` (15
seconds); the CLI sets this from `-lock-timeout` before opening the driver.
A shorter driver statement timeout takes precedence. Library callers must set
the default before construction; changing an instance's `LockTimeout` afterward
does not change initialization.

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

Normal tests use SQL mocks; they need no database. Real-server tests are
opt-in and destructive:

```powershell
$env:MIGRATE_PLUS_OPENGAUSS_TEST_URL = 'opengauss://user:password@host:5432/testdb?search_path=disposable&sslmode=disable&x-statement-timeout=15000'
go test ./database/opengauss -run TestIntegration -count=1 -v
```

Use an empty disposable schema. The suite reuses upstream conformance tests and
checks lock survival through DDL/commit/rollback, version durability, defensive
transaction cleanup, dirty force/down recovery, timeout disposal and dependency
Drop. Set `MIGRATE_PLUS_OPENGAUSS_TEST_OTHER_URL` to a second disposable schema
in the same database for cross-schema dependency checks; the account needs
sufficient privileges in both schemas. Tests must not run concurrently on
those schemas.

Record exact server build, mode and topology when running these tests. They
have not been run against openGauss here. SHA256/SM3 authentication, distributed
locking, failover and other compatibility modes remain unverified and are not
enabled.
