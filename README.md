Postlite
========

Postlite is a network proxy to allow access to remote SQLite databases over the
Postgres wire protocol. This allows GUI tools to be used on remote SQLite
databases which can make administration easier.

The proxy works by translating Postgres frontend wire messages into SQLite
transactions and converting results back into Postgres response wire messages.
Many Postgres clients also inspect the `pg_catalog` to determine system
information so Postlite mirrors this catalog by using an attached in-memory
database with virtual tables. The proxy also performs minor rewriting on these
system queries to convert them to usable SQLite syntax.

Postlite implements enough of the PostgreSQL 14 wire protocol — real type OIDs,
transactions, a populated `pg_catalog`/`information_schema`, command tags, and DDL
translation — that strict client libraries (pgx, node-postgres, JDBC) and GUI
tools can connect, introspect the schema, read and edit rows, and manage tables as
if they were talking to a real PostgreSQL instance.


## Usage

To use Postlite, execute the command with the directory that contains your
SQLite databases:

```sh
$ postlite -data-dir /data
```

On another machine, you can connect via the regular Postgres port of 5432:

```sh
$ psql --host HOSTNAME my.db
```

This will connect you to a SQLite database at the path `/data/my.db`.


## Docker

The image bundles the server so you can mount a single SQLite file and connect to
it over the Postgres protocol with a username and password.

```sh
$ docker run -d --name postlite -p 5432:5432 \
    -e POSTLITE_DATABASE=/data/app.db \
    -e POSTLITE_USER=postgres \
    -e POSTLITE_PASSWORD=secret \
    -v "$PWD/app.db:/data/app.db" \
    malayh/postlite:latest
```

(To build the image yourself instead of pulling it, run `docker build -t
malayh/postlite:latest .` first, or `make build-image`.)

Then connect with any Postgres client. In single-file mode the database name in
the connection string is arbitrary — every connection is served the mounted file:

```sh
$ psql "postgres://postgres:secret@localhost:5432/app?sslmode=disable"
```

If `app.db` does not exist yet it is created on first connect. Mounting the
*directory* (`-v "$PWD/data:/data"`) instead of the bare file lets SQLite also
create its `-wal`/`-journal` siblings next to the database.

A `docker-compose.yml` is included for a one-command start:

```sh
$ docker compose up
```

### Configuration

All options are read from the environment (CLI flags override them):

| Variable            | Flag         | Default     | Description                                                        |
| ------------------- | ------------ | ----------- | ------------------------------------------------------------------ |
| `POSTLITE_DATABASE` | `-database`  | —           | A single `.db` file served to every connection (overrides below). |
| `POSTLITE_DATA_DIR` | `-data-dir`  | `/data`     | Directory of `.db` files; the database name selects a file.       |
| `POSTLITE_USER`     | `-username`  | `postgres`  | Username clients authenticate as (when a password is set).        |
| `POSTLITE_PASSWORD` | `-password`  | —           | Password clients must supply. **Empty disables authentication.**  |
| `POSTLITE_ADDR`     | `-addr`      | `:5432`     | Bind address.                                                      |


## Authentication

When a password is configured (`-password` / `POSTLITE_PASSWORD`), clients must
authenticate using PostgreSQL's MD5 password scheme, which is supported out of the
box by `psql`, `pgx`, node-postgres, JDBC, and other mainstream drivers. When no
password is set, the server accepts every connection (the original behavior).

> Authentication only protects who may open a session; the wire protocol itself is
> not encrypted (the server responds to `SSLRequest` with "unsupported"). Run it on
> a trusted network or behind a TLS-terminating tunnel.


## Development

Postlite uses virtual tables to simulate the `pg_catalog` so you will need to
enable the `vtable` tag when building:

```sh
$ go install -tags vtable ./cmd/postlite
```

