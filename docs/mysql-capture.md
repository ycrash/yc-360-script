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
server's: version checks take MariaDB's numbers for MySQL's.

`mysql:` is a list of targets and takes exactly one, with a required `id`, as
`postgres:` does. A file may hold a `postgres:` block or a `mysql:` block, never
both: a run captures one database.

`port` defaults to `3306` and `tls` to an encrypted connection whose
certificate is not checked. `captureDuration` is required and capped at `2h`;
`frequency` defaults to `5m`, is floored at `5s`, and gives the three speeds
the same way (fast at most 15s, expensive at least 5m).

A key the block does not take is refused at startup with its line, inside `tls:`
too, as for `postgres:`. Ctrl-C or `SIGTERM` ends the window early as it does
there: each file closes with `status=cancelled` and is uploaded, and a second
signal exits at once.

`database` is optional, and nothing defaults it: every file reads
`performance_schema` and the server's counters, which cover the whole
instance whichever schema the connection is in. Named, it is the connection's
default schema, and the role must be allowed to use it: a name it may not use
refuses the connection (`Error 1044`), and every file records
`connect_failed`.

`password` takes `${NAME}` references, as for `postgres:`: each is replaced
at startup with that environment variable's value, and one that is unset or
empty is refused. Only the braced form is read: `$NAME` is sent as written, as
part of the password.

The `-c` file must be readable by its owner alone, as for `postgres:`.

### `tls`

The same block as `postgres:`, mapped to the MySQL driver:

| `tls:` | Connection |
|---|---|
| omitted, or `enabled: true` | encrypted, certificate not checked |
| `verifyServerCertificate: true` | encrypted; the certificate must chain to `caFile` (or the system's trust store) and name `serverName` (or `host`) |
| `enabled: false` | plaintext, with a startup warning |

The agent never falls back from TLS to plaintext. `caFile` is read at startup:
a file that cannot be read or holds no PEM certificate is refused there, and a
relative path is from the directory the agent was started in.

**The server's own certificate cannot be verified.** The certificate MySQL
generates for itself on first start - what the official image runs with - has
no subject alternative names, and its Common Name is
`MySQL_Server_<version>_Auto_Generated_Server_Certificate` rather than a host.
The agent checks the server's name against subject alternative names only, so
with `verifyServerCertificate: true` the connection to such a server fails
whatever `caFile` and `serverName` say. Verification needs a certificate issued
with the server's name, or IP address, among its subject alternative names. The
default, encrypted and unverified, works with the generated certificate.

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
no plans in its log to copy. A digest first seen after the fifth plan is written
with `reason=plan_limit_reached`. A plan that waits out the 2s lock-wait bound -
behind a queued `ALTER TABLE`, say - is `status=TIMEOUT`, with the server's error
in `error=`, and the wait counts once as error 1205 in the server's own
statistics.

A statement is read past the comments it opens with, so an application that
puts one before every statement (Hibernate's `use_sql_comments`, sqlcommenter)
still gets plans. The session is read-only, and MySQL refuses `EXPLAIN` of an
`UPDATE`, `DELETE`, `INSERT` or `REPLACE` there, so write statements get no
plan (`reason=read_only_session`), a `WITH ... UPDATE` or `WITH ... DELETE`
included. A prepared statement's sample keeps its `?` markers, which no server
can plan, so it gets none either (`reason=placeholder`). EXPLAIN needs `SELECT`
on the tables the statement reads.

A plan is asked for in the schema its statement ran in. On 8.4 and later that
is `EXPLAIN ... FOR SCHEMA`, which leaves the session where it was; 8.0 has no
such form, so the connection `USE`s the schema for the plan and then goes back
to the configured `database`, or with none to `information_schema`. The server
files every statement under the schema it ran in, so with no `database` the
agent's own statements are not filed under the application's schema; with one,
they are filed under it, the connection's default.

### `agentOnDbHost`

As for `postgres:`. The measurement it stands in for: a `mysqld` or `mariadbd`
process on this machine that started when the connected server says it did
(`now - Uptime`, within 2s; `agent_on_db_host_by=process_start_time`). Or one
that is older than the server while its threads are not
(`thread_start_time`): the official Docker image's entrypoint initialises the
data directory and then execs `mysqld`, which keeps the entrypoint's start, so
after a first boot the process is older than the server by the init's length
for the container's whole life. An agent in a container of its own sees no
process beside it and says `unknown`, `reason=container`; one that shares the
server container's process table (`--pid=container:<name>`) can say yes.

## A database capture is its own run

As for `postgres:`. A `mysql:` block cannot run with `-m3`: M3 database
monitoring polls PostgreSQL only, and the agent refuses the combination at
startup rather than run a monitoring loop that captures nothing of the
database.

**A mis-nested block is silent when an application target is present.** The
block belongs under `options:`; at the top level of the file it decodes to
nothing. On a database-only run that is loud - the run has no target left and
stops with `nothing can be done`. Alongside `-p` it is not: the run has a
target, so it proceeds and captures no database. Check the nesting.

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

Most files also need `performance_schema` on. A server started with
`performance_schema=OFF` keeps its tables, but the ones its instruments fill
answer with no rows. Each block that reads one is then not asked: it says
`reason=performance_schema_off`, with no rows, which the spec calls data not
available because a required setting is off, not an error. The status and
variable reads, `information_schema.innodb_trx`, `my_buffer_pool.txt`,
`my_storage.txt` and `my_nondefault_settings.txt` are unaffected.
`my_replication.txt`'s mechanism is `unknown` unless the server is a replica,
and `my_metadata.txt` says `cpu_time_reason,performance_schema_off`.

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
  was found, `unknown` where a check was refused or failed. Galera is a node
  with a provider loaded, which `wsrep_provider_name` names, and not any
  `wsrep_` row as the spec has it: MariaDB reports the base variables
  (`wsrep_cluster_size` 0, `Disconnected`) with none loaded, and a node cut off
  from its cluster reports much the same status, still naming its provider.
- `my_prepared_statements.txt` — every prepared statement, by owning thread.
- `my_slow_queries.txt` — the 100 digests with the most time, keyed by
  `(schema_name, digest)`. A column this server cannot fill - `sum_cpu_time`
  before 8.0.28 or with its consumer off, the memory columns before 8.0.31 or
  with the `memory/%` instruments off, when the server reads zero for them - is
  empty and named in `unavailable=`, never written as zero.
- `my_explain.txt` — see *`explain`*.

Every 10s, whatever `frequency` is, on the normal-speed connection:

- `my_deadlocks.txt` — the error log's tail: see *Deadlocks*.

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
The three top-100 reads - `my_slow_queries.txt`, `my_table_stats.txt` and
`my_storage.txt` - also say how many rows there were before the 100, as
`rows_total=`, and `truncated=true` where that is more than they wrote.
`my_deadlocks.txt`, a log tail, keeps one header line per block, as
`pg_deadlocks.txt` does.

## Deadlocks

`my_deadlocks.txt` copies every deadlock report the error log gains during the
window, read from its end at the first sample and then every 10s, at most 16 MB
a read. Two things must both hold for there to be any:

- `innodb_print_all_deadlocks=ON`, which makes InnoDB write every deadlock to
  the error log; and
- `log_access=direct`: the agent can open the file `@@log_error` names. The
  official Docker image logs to `stderr`, which is no file at all. Where the
  open fails, `my_metadata.txt`'s `log_open_error` says how: `not_found`, a
  path the agent cannot see (the server's, in another container or machine),
  or `permission_denied`, a file it may not read. MySQL creates its log 0640,
  owned by its own account, so an agent on the database host is refused it
  unless it runs in the server's group.

Without them the only deadlock the server shows is its most recent, in `SHOW
ENGINE INNODB STATUS`'s LATEST DETECTED DEADLOCK, which `my_buffer_pool.txt`
already captures every sample; the deadlock file's blocks say so
(`fallback=`), whichever of the two is missing. With the setting off and the
log readable, the log is still read, in case the setting is turned on during
the window. On Amazon RDS and Aurora the log is not a file the agent can read.

## What leaves the database

Statement text keeps its statement and loses its values, in every file that
carries it: `PROCESSLIST_INFO`, `query_sample_text`, `SQL_TEXT`, and the
statements in deadlock reports and `SHOW ENGINE INNODB STATUS`. A string becomes
`'<redacted>'` inside its own quotes and prefix, a number `<redacted>`. Where a
string ends depends on the session's `NO_BACKSLASH_ESCAPES`, which no statement
records: `'O\'Brien'` is one string with it off and a string and a word with it
on. A string the two readings end at different quotes, as they do that one, is
replaced to the end of the statement, since in one of them what follows it is a
value. A statement that sets a credential - `CREATE USER`, `ALTER USER`, `SET
PASSWORD`, `CHANGE REPLICATION SOURCE` and the like - is replaced past its first
two words.
InnoDB's dumps print each locked row's bytes as hex and text; both are replaced.

A replica's last errors (`Last_Error`, `Last_SQL_Error`, `Last_IO_Error`) are the
server's messages, not SQL, and the server quotes values into them without
escaping their quotes. Everything from a message's first quote to its last is
replaced; the error codes, worker numbers, log files and positions around it
stay. A message whose only quoted text is a GTID - the parallel applier's
"Worker 1 failed executing transaction '…'" - is kept whole, since it names the
transaction to skip or repair. A statement-based failure's `Query: '…'` keeps its
statement, with the values replaced as above. A plan keeps its table, index and column
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
`/* yCrash-DB-Agent */`, the opening `SET` included, which names its variables
in the same order every time and so is one digest.

## A connection lost mid-capture

As for `postgres:`, each connection is held for the whole window and never
reconnected: a new one would restart every delta baseline under the same
artifact. When the driver closes one, that connection's timeline stops at the
sample that found out, and every file sampled on it closes with
`status=connection_lost`, a `samples_written` below `samples_expected`, and the
driver's error in `connection_error=`. A file that already had all its samples -
`my_metadata.txt`, read once at the start - closes `complete`. The files on the
other two connections go on to the end of the window, and everything written is
in the bundle.

Besides a killed session, a broken network or a failover, a statement the server
has not answered within 10s - the agent's own deadline - closes its connection,
because the driver can end a statement only by closing the socket. A `SELECT`
does not get there: `max_execution_time` ends it at 5s with error 3024, the block
says `status=TIMEOUT`, and the connection stays open. A `SHOW` or `EXPLAIN` has
no server-side bound on its running time, only `lock_wait_timeout` on a wait,
and on a server that has stopped answering no statement has any. The server runs a statement the agent gave up on to its end, and the lost
connection counts once in `Aborted_clients`, which `my_timeouts.txt` reads.

So a stall ends the window for each connection with a statement waiting 10s on
it, and only those. With the server paused for 12s, 8s into a 30s window, the
fast connection's statement was in flight when the pause began: it reached the
deadline (`duration_ms=10002`), and both fast files closed `connection_lost`
with 2 of 9 samples. The normal and expensive connections sent their next
statements 4s into the pause and were answered 8s later; their files skipped
one tick and closed `status=partial`.

`connection_error=` says why the connection ended. A statement that reached the
10s deadline says `context deadline exceeded`, or `invalid connection: read tcp
…: i/o timeout` when the socket's own read deadline fired first. A session the
server closed - a `KILL`, a restart - says `invalid connection: unexpected EOF`.
