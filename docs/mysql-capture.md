# MySQL capture — setup

A `mysql:` block under `options:` in the `-c` YAML file selects a MySQL server as
a capture target. It works as the `postgres:` block does (see
`postgres-capture.md`): there is no CLI flag, presence of the block is the
switch, and credentials never go on the command line.

```yaml
options:
  mysql:
    - id: orders-primary
      host: db.internal
      port: 3306
      username: yc_monitor
      password: ${YC_MYSQL_PASSWORD}
      tls:
        verifyServerCertificate: true
        caFile: /etc/ycrash/ca.pem
      captureDuration: 120s
      frequency: 30s
```

Supported: MySQL 8.0, 8.4 LTS and 9.x, and Percona Server. MariaDB is not: its
`performance_schema` lacks tables the capture reads. A run against one still
connects, with MariaDB's names for the same session bounds, and writes
`my_metadata.txt` with `mysql_variant=mariadb`; each other file records what the
server refused or lacks. Read nothing else in such a bundle as a MySQL
server's: version checks take MariaDB's numbers for MySQL's, and
`my_replication.txt` reports any MariaDB as Galera.

`mysql:` is a list of targets and takes exactly one, with a required `id`, as
`postgres:` does. A file may hold a `postgres:` block or a `mysql:` block, never
both: a run captures one database.

`port` defaults to `3306` and `tls` to an encrypted connection whose
certificate is not checked. `captureDuration` is required and capped at `2h`;
`frequency` defaults to `5m`, is floored at `5s`, and gives the three speeds
the same way (fast at most 15s, expensive at least 5m).

`database` is optional, and nothing defaults it: every file reads
`performance_schema` and the server's counters, which cover the whole
instance whichever schema the connection is in. Named, it is the connection's
default schema, and the role must be allowed to use it: a name it may not use
refuses the connection (`Error 1044`), and every file records
`connect_failed`.

The `-c` file must be readable by its owner alone, as for `postgres:`.

### `tls`

The same block as `postgres:`, mapped to the MySQL driver:

| `tls:` | Connection |
|---|---|
| omitted, or `enabled: true` | encrypted, certificate not checked |
| `verifyServerCertificate: true` | encrypted; the certificate must chain to `caFile` (or the system's trust store) and name `serverName` (or `host`) |
| `enabled: false` | plaintext, with a startup warning |

The agent never falls back from TLS to plaintext.

**Plaintext and `caching_sha2_password`.** MySQL 8's default authentication
needs, over a plaintext connection, the server's RSA public key - unless the
server has the account's password in its cache. A client that asks the server
for its key can be handed a man-in-the-middle's key instead, and then sends the
password encrypted for the attacker. The agent never asks: its MySQL driver
would, so the agent refuses that one request on the wire and fails the login
with

```
connect_error=public_key_retrieval_refused
```

and a log line saying to enable TLS for the target. Because the server caches a
password after the account's last login over TLS (until a restart or `FLUSH
PRIVILEGES`), a plaintext capture can succeed one day and fail the next. A wrong
password over plaintext ends the same way: the server asks for full
authentication whenever its quick check fails.

### `explain`

Omitted, no plans are captured. `explain: all` captures one plan per statement
digest, at most five per capture: `EXPLAIN FORMAT=JSON` of the digest's
`query_sample_text`, which is the application's statement **with its real
values**, sent back to the database. The plans written to the bundle have their
values replaced. `logged` is refused: MySQL has no `auto_explain`, so there are
no plans in its log to copy.

The session is read-only, and MySQL refuses `EXPLAIN` of an `UPDATE`, `DELETE`,
`INSERT` or `REPLACE` there, so write statements get no plan
(`reason=read_only_session`). A prepared statement's sample keeps its `?`
markers, which no server can plan, so it gets none either
(`reason=placeholder`). EXPLAIN needs `SELECT` on the tables the statement
reads.

### `agentOnDbHost`

As for `postgres:`. The measurement it stands in for: a `mysqld` or `mariadbd`
process on this machine that started when the connected server says it did
(`now - Uptime`, within 2s).

## A database capture is its own run

As for `postgres:`. A `mysql:` block cannot run with `-m3`: M3 database
monitoring polls PostgreSQL only, and the agent refuses the combination at
startup rather than run a monitoring loop that captures nothing of the
database.

## Which role to use

```sql
CREATE USER 'yc_monitor'@'%' IDENTIFIED BY '...' REQUIRE SSL;
GRANT PROCESS, REPLICATION CLIENT ON *.* TO 'yc_monitor'@'%';
GRANT SELECT ON performance_schema.* TO 'yc_monitor'@'%';
```

- `PROCESS` shows every session in `my_sessions.txt` (without it, only the
  agent's own), and is needed for `information_schema.innodb_trx` and `SHOW
  ENGINE INNODB STATUS`.
- `REPLICATION CLIENT` is needed for `SHOW REPLICA STATUS` and the binary log
  status.
- `SELECT` on `performance_schema` is needed for every other file.
- `my_storage.txt` lists only tables the role holds some privilege on. For the
  application's tables to appear, grant one on its schemas, for example
  `GRANT SELECT ON orders_db.* TO 'yc_monitor'@'%'` - which `explain: all` needs
  anyway.

A read the role may not make is not an error. Its block says
`reason=permission_denied`, the file is `status=complete`, and the read is asked
once per capture and remembered: every refusal counts once more in the server's
own error statistics, which `my_errors.txt` reads.

## What a capture writes

Every artifact is a `my_*.txt` file in the bundle, uploaded under a type of its
own (`myMeta`, `mySessions`, and so on). The receiver may not know these types
yet; one it does not know accepts the upload and discards it with no error at
either end, so a reported success is not yet proof of arrival. The bundle
carries the files regardless.

Read once, at the start:

- `my_metadata.txt` — the target as configured; the server's version,
  `version_comment` and `mysql_variant`; its uptime; the settings the other
  files depend on; `log_access` and `has_mysql_error_log_access`;
  `has_cpu_time_instrument_enabled` (with `cpu_time_reason` when false),
  `has_statement_memory_tracking`, `has_memory_instruments_enabled`; the role's
  `has_process`, `has_replication_client` and `has_performance_schema_select`
  (direct grants; `enabled_roles` lists any active role, whose privileges are
  not listed); the base tables by engine (`innodb_tables_share`, among the
  tables the role can see); and the same-host verdict.

At the fast speed:

- `my_sessions.txt` — `performance_schema.threads` with each session's
  `program_name` (the agent's own are `yCrash-DB-Agent`), at most 1000; and
  `information_schema.innodb_trx`.
- `my_lock_waits.txt` — `data_lock_waits`, and pending `metadata_locks`.

At the normal speed:

- `my_capacity.txt` — `Threads_connected`, `Threads_running`,
  `Aborted_connects`, `Questions`, `Threads_cached`, `Threads_created` and
  `Connections`. `Threads_connected` includes the agent's three connections.
- `my_errors.txt` — `events_errors_summary_global_by_error`, every error raised
  at least once.
- `my_timeouts.txt` — the counts of errors 1205 (lock wait timeout), 3024
  (`max_execution_time`) and 4031 (session closed for inactivity), and
  `Aborted_clients`. **Not a log tail**, as the spec has it: the server writes
  none of these to its error log.
- `my_memory.txt` — the buffer pool's size and pages, the temporary-table spill
  counters, memory by connection, and the prepared-statement, InnoDB and
  Performance Schema memory instruments. With `memory/%` instruments off, the
  instrument blocks read zeros.
- `my_buffer_pool.txt` — `SHOW ENGINE INNODB STATUS`, as text (`format=text`,
  `bytes=`).
- `my_replication.txt` — the replica's status; failing that, the Group
  Replication members; failing that, Galera's `wsrep_%` status. Then the binary
  log's position and retention. `mechanism=` on the binary log block says which
  was found, `unknown` where a check was refused.
- `my_prepared_statements.txt` — every prepared statement, by owning thread.
- `my_slow_queries.txt` — the 100 digests with the most time, keyed by
  `(schema_name, digest)`. A column this server cannot fill - `sum_cpu_time`
  before 8.0.28 or with its consumer off, the memory columns before 8.0.31 - is
  empty and named in `unavailable=`, never written as zero.
- `my_explain.txt` — see *`explain`*.
- `my_deadlocks.txt` — continuous: see *Deadlocks*.

At the expensive speed:

- `my_nondefault_settings.txt` — every variable whose source is not
  `COMPILED`, with its current value.
- `my_table_stats.txt` — the 100 most-read index uses; an empty `INDEX_NAME` is
  a full scan.
- `my_storage.txt` — the 100 largest tables outside the system schemas. The
  sizes are the server's cached statistics, up to
  `information_schema_stats_expiry` old (a day by default); the agent does not
  ask for fresh ones, which would open every table.

Every sample block of a periodic file carries the same two header lines as the
`postgres:` files (`capture_id`, `target_id`, `engine=mysql`, `engine_version`,
then the sample line), with `db=` the connection's default schema.
`my_deadlocks.txt`, a log tail, keeps one header line per block, as
`pg_deadlocks.txt` does.

## Deadlocks

`my_deadlocks.txt` copies every deadlock report the error log gains during the
window, read from its end at the first sample. Two things must both hold for
there to be any:

- `innodb_print_all_deadlocks=ON`, which makes InnoDB write every deadlock to
  the error log; and
- `log_access=direct`: the agent can open the file `@@log_error` names. The
  official Docker image logs to `stderr`, which is no file at all.

Without them the only deadlock the server shows is its most recent, in `SHOW
ENGINE INNODB STATUS`'s LATEST DETECTED DEADLOCK, which `my_buffer_pool.txt`
already captures every sample; the deadlock file's blocks say so
(`fallback=`). On Amazon RDS and Aurora the log is not a file the agent can
read.

## What leaves the database

Statement text keeps its statement and loses its values, in every file that
carries it: `PROCESSLIST_INFO`, `query_sample_text`, `SQL_TEXT`, the replica's
last errors, and the statements in deadlock reports and `SHOW ENGINE INNODB
STATUS`. A string becomes `'<redacted>'` inside its own quotes and prefix, a
number `<redacted>`. A statement that sets a credential - `CREATE USER`,
`ALTER USER`, `SET PASSWORD`, `CHANGE REPLICATION SOURCE` and the like - is
replaced past its first two words. InnoDB's dumps print each locked row's bytes
as hex and text; both are replaced. A plan keeps its table, index and column
names, its access types, and its costs and row estimates; every other string in
it has its values replaced - the conditions, each step's operation, and the
rewritten statement MySQL 9's plan format carries whole. A key a later version
adds is replaced until it is known to hold only names. Each block counts its
replacements as `redacted=`.

## What each connection is allowed to do

Each of the three connections opens with, in one statement before any
collector runs: `max_execution_time` of 5s (SELECT only), `lock_wait_timeout`
and `innodb_lock_wait_timeout` of 2s, `transaction_read_only`, a `wait_timeout`
longer than the longest window, and `time_zone='+00:00'`. Statements carry no arguments, so none is
prepared server-side and the agent never appears in its own
`my_prepared_statements.txt`. Every statement starts with
`/* yCrash-DB-Agent */`.
