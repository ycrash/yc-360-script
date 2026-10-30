# MySQL capture matrix — spike results

`my_matrix_init.sql` and `my_replica_init.sql` are not fixtures: they are the
roles, tables and replication setup `compose.mysql.yaml` mounts into each
container for the opt-in matrix (`-tags mysqlintegration`), and they live here
because they are test input the package owns. The `my_*.txt` files and the
`my84_*.json` recordings they are made from are the goldens; see *Goldens*, at
the end.

This file is step 1 of `mysql-capture-plan.md`'s order of work: what the spec's
queries do on real servers, recorded so the code that depends on them does not
rest on belief. Measured 2026-10-27 on the two 8.0 servers, and 2026-10-30 on
all five.

## What was run

| Server | Image | Version |
|---|---|---|
| `my80` | `mysql:8.0` | 8.0.46 |
| `my80old` | `mysql:8.0.27` (amd64, emulated on Apple Silicon) | 8.0.27 |
| `my84` | `mysql:8.4` | 8.4.13 |
| `my9` | `mysql:9` | 9.7.4 |
| `percona84` | `percona/percona-server:8.4` | 8.4.11-11 |

`go test -tags mysqlintegration` passes on all five, every role:
`TestMatrixWindow` (every file, all three roles), `TestMatrixScenarios` (the
staged cases below), `TestMatrixPlaintextLoginNeverRequestsTheServersKey`, and
`TestMatrixVerifiedTLS` on `my84`. A finding marked *8.0 only* was probed by
hand on the two 8.0 servers; the matrix asserts the parts of it that the
recommended roles depend on everywhere.

The `replica`, `perms` and `mariadb` profiles ran on 2026-10-30; see their
sections below.

## The plan's *(verify)* statements

- **The 8.0.27 image is amd64-only.** Confirmed: compose needs
  `platform: linux/amd64` for it on Apple Silicon. It runs under emulation.
- **`UMASK: "0644"` makes `error.log` readable from the host.** Confirmed: the
  file is `-rw-r--r--`. The data directory's own files are unaffected.
- **Cancelling a context closes the socket.** Confirmed in the driver's source
  (`watchCancel`): the connection is marked closed, and `Conn.Lost()` reports
  it, so a context deadline mid-statement ends that connection's timeline - the
  same behaviour pgx has, and the reason `StatementDeadline` sits above
  `max_execution_time`.
- **A statement whose client gave up keeps running on the server.** Measured on
  all five, on the agent's own connection with a 1s client deadline:
  `SELECT SLEEP(4)` ran its 4s, and a `BENCHMARK` its whole uncancelled time
  (2s; 5s on the emulated 8.0.27). The server notices the closed socket only
  when the statement ends and it reads the next command. Each such loss adds 1
  to `Aborted_clients`, which `my_timeouts.txt` reports, and at
  `log_error_verbosity=3` (`my9`) writes an "Aborted connection ... Got an
  error reading communication packets" note; at 2, none. So a SELECT is bounded
  by `max_execution_time` alone, and a SHOW or EXPLAIN that outlives
  `StatementDeadline` runs on unbounded after the agent has given up on it.
- **Public key retrieval.** The plan assumed an `allowPublicKeyRetrieval`
  switch to leave off. go-sql-driver has none: over plaintext it always asks the
  server for its RSA key when `caching_sha2_password` or `sha256_password` wants
  full authentication. The agent therefore guards the plaintext socket and
  refuses that one packet (`keyRequestGuard`), failing the login with
  `connect_error=public_key_retrieval_refused`. Measured: after `FLUSH
  PRIVILEGES` the plaintext login is refused; right after a TLS login by the
  same account it succeeds, because the server caches the hash - the
  intermittent failure the plan predicted. A wrong password over plaintext also
  ends in the key request, since the server asks for full authentication
  whenever its quick check fails, and the advice says so.
- **The auto-generated certificate has no subject alternative name.**
  Confirmed on `percona84`: "No extensions in certificate", subject
  `CN=MySQL_Server_8.4.11-11_Auto_Generated_Server_Certificate`. Go's verifier
  cannot accept it, so verified TLS needs a real certificate; `my84` serves
  the setup script's, which names `127.0.0.1` and `localhost`, and verifies.
  A `serverName` the certificate does not carry is refused.
- **The public key guard on every version.** After `FLUSH PRIVILEGES`, a
  plaintext login is refused before the key request on all five servers.
- **MariaDB lacks the tables the spec relies on.** Confirmed on 10.11.6, with
  `performance_schema` on: `data_lock_waits`, `variables_info` and
  `events_errors_summary_global_by_error` do not exist (1146), and
  `events_statements_summary_by_digest` has no `query_sample_text` (1054). But
  the run never got that far at first; see the MariaDB section.

## Privileges, per role (8.0 only)

| Read | `root` | `yc_monitor` | `yc_restricted` |
|---|---|---|---|
| `performance_schema.*` | yes | yes | 1142, except below |
| `performance_schema.global_variables`, `variables_info` | yes | yes | **yes**: readable with no grant |
| `information_schema.innodb_trx` | yes | yes | 1227 (PROCESS) |
| `SHOW ENGINE INNODB STATUS` | yes | yes | 1227 (PROCESS) |
| `SHOW REPLICA STATUS`, `SHOW MASTER STATUS` | yes | yes | 1227 (REPLICATION CLIENT) |
| `information_schema.TABLES`, application schemas | all | **none** | none |
| `SHOW GLOBAL STATUS`, `@@log_error` | yes | yes | yes |

- **`information_schema.TABLES` lists only tables the role holds a privilege
  on.** `yc_monitor`, the recommended role, sees none of the application's
  tables: `my_storage.txt` is empty for it, and `my_metadata.txt`'s
  `tables_visible` is 0. The setup documentation recommends a grant on the
  application schemas for that reason.
- **A configured `database:` the role may not use refuses the connection**
  (1044), and the window records `connect_failed` on every file without
  dialling again: it is the target's failure, not a slot's.
- **The agent's own refusals are counted by the server.** Each refused read
  adds one to `events_errors_summary_global_by_error` under 1142 or 1227, which
  `my_errors.txt` reads. So a refused block is asked once per capture and
  remembered (`reason=permission_denied` on every later sample, with no
  statement run), rather than adding one every sample.
- `information_schema.USER_PRIVILEGES` lists only the account's direct grants,
  and every account's rows for a superuser: `my_metadata.txt` filters to
  `CURRENT_USER()`. A privilege held through a role is not listed;
  `enabled_roles` says when that may be the case.

## Capability flags

Asserted by `TestMatrixWindow` for the two roles that may read
`performance_schema`.

| Flag | `my80` | `my80old` | `my84` | `my9` | `percona84` |
|---|---|---|---|---|---|
| `has_cpu_time_instrument_enabled` | false, `consumers_off` | false, `version_before_8.0.28` | true | true | false, `consumers_off` |
| `has_statement_memory_tracking` | true | false | true | true | true |
| `has_memory_instruments_enabled` | false (`memory/%=OFF`) | true | true | true | true |

- `--performance-schema-instrument=memory/%=OFF` leaves the 76
  `memory/performance_schema/%` instruments on: they cannot be switched off.
  `has_memory_instruments_enabled` counts every other `memory/%` instrument.
  With them off, the memory summaries read zeros, not NULLs.
- On 8.0.27 the `events_statements_cpu` consumer does not exist, and
  `sum_cpu_time`, `max_total_memory` and `max_controlled_memory` are not
  columns: `my_slow_queries.txt` selects `NULL` in their place and names them
  as `unavailable=`.
- With the consumer on (`my84`, `my9`), `sum_cpu_time` is read and nothing is
  named `unavailable=`.

## The spec questions (plan §6)

1. **`my_timeouts.txt`'s source.** Settled: error 1205 (lock wait timeout) and
   3024 (`max_execution_time` exceeded) are returned to the client and written
   nowhere in the error log, at verbosity 2 or 3. Both are counted in
   `events_errors_summary_global_by_error`. `my_timeouts.txt` therefore reads
   those counters (1205, 3024 and 4031) and `Aborted_clients`, at normal
   speed, instead of tailing the log. Not measured: the "Aborted connection"
   note an idle session's end writes at verbosity 3.
2. **`performance_schema.error_log` as a deadlock fallback.** Settled, no: at
   verbosity 2 it receives nothing from a deadlock; at verbosity 3 it receives
   the `[Note]` framing lines (`MY-012468` and `MY-012469`) but none of the
   statements or record dumps, which InnoDB writes past the log sink. It can
   count deadlocks, not describe them. The fallback stays `SHOW ENGINE INNODB
   STATUS`'s LATEST DETECTED DEADLOCK, which `my_buffer_pool.txt` already
   captures.
3. **`hasMysqlErrorLogAccess`.** Defined as Postgres defines log access:
   `@@log_error` resolved as the server would (absolute, or relative to
   `@@datadir`), then opened by this process. `stderr` - the official image's
   default - is `log_access=none reason=stderr`.
   **The deadlock report has two shapes**, and the tail reads both: at
   verbosity 3, a `[Note] [MY-012468]` line and `[MY-012469]` section
   headings around InnoDB's dump; at the default verbosity of 2 the notes are
   filtered and the dump alone remains, starting at a bare `TRANSACTION <id>,
   ACTIVE` line with no `deadlock` text anywhere. The dump prints each locked
   row as hex and text (` 4: len 1; hex 61; asc a;;`), which the redaction
   replaces.
4. **`information_schema_stats_expiry`.** 86400 by default, so
   `my_storage.txt`'s sizes may be a day old. Left alone: setting it to 0 opens
   every table. `my_metadata.txt` records the value.
5. **Replication statements by version.** `SHOW REPLICA STATUS` from 8.0.22,
   `SHOW SLAVE STATUS` before; `SHOW BINARY LOG STATUS` from 8.2, `SHOW MASTER
   STATUS` before. On 8.0.46 `SHOW MASTER STATUS` still works; on 8.4.13, 9.7.4
   and Percona 8.4.11 it is a syntax error (1064), and `SHOW BINARY LOG STATUS`
   answers.
6. **`my_nondefault_settings.txt`.** The joined query is written:
   `variables_info` LEFT JOIN `global_variables`, so a session-only variable
   keeps an empty value.
7. **`my_metadata.txt`'s flags.** All written; see the table above.
8. **Same-host detection.** MySQL is one process, so the test is the server's
   own start: a local `mysqld` or `mariadbd` whose start time is within 2s of
   `now - Uptime`. On this machine Docker runs the servers in a VM, and the
   verdict is `no reason=no_server_process`, which is right. The yes path is
   unit-tested only.

## Staged cases (`TestMatrixScenarios`, all five)

Before the window, the scenario truncates the digest and index-usage summaries
(test setup only: the agent never writes to the server) and runs a slow
application `SELECT` and twenty scans of the table with no index. During it, a
transaction holds a row with `FOR UPDATE`; a second session waits for that row;
an `ALTER TABLE` queues for the metadata lock behind the transaction; a session
holds a server-side prepared statement with a literal in it; and a statement
runs past `max_execution_time`. Every file is `status=complete`, and:

- `my_lock_waits.txt`: a fast sample has `data_lock_waits` rows and a pending
  `metadata_locks` row naming `yc_app.orders`.
- `my_prepared_statements.txt`: the held statement, `status = '<redacted>'`,
  its `?` kept, and none of the agent's own.
- `my_timeouts.txt`: 1205 rises by at least two between the opening and closing
  samples (the row lock and the metadata lock both end in it) and 3024 by at
  least one.
- `my_explain.txt`: the application `SELECT` is planned under
  `schema_name=yc_app`, with no value of it left in the plan.
- `my_table_stats.txt`: the scanned table's row has an empty `INDEX_NAME`.

## Replication (`replica` profile, 8.4.13)

`my84-replica` replays `my84` from its first GTID and catches up at once.
`TestReplica*` skip when the profile is not up, and leave the replica running
and caught up.

- **Running:** `mechanism=async`, both threads `Yes`, `Seconds_Behind_Source`
  0. The source itself, with a replica attached, reads `mechanism=none` with
  its binary log status: the spec's test is what a server replicates from.
- **Applier stopped** (`STOP REPLICA SQL_THREAD`): still `mechanism=async`,
  `Seconds_Behind_Source` empty (NULL).
- **A replication error** - a row inserted on the replica, then the same key on
  the source - is worded three ways, and the agent's redaction was wrong on two:
  - The default since 8.0.27, a parallel applier with row events: `Last_Error`
    and `Last_SQL_Error` are the coordinator's summary, "Worker 1 failed
    executing transaction '<GTID>' at source log binlog.000002, end_log_pos
    1102". No value is in it; the duplicate key is only in
    `performance_schema.replication_applier_status_by_worker`, which the spec
    does not read. The SQL redactor replaced the worker number, the GTID, the
    log file's number and the position: everything an operator needs to skip or
    repair the transaction, none of it a value.
  - A single applier (`replica_parallel_workers=0`) with row events: "Could not
    execute Write_rows event on table yc_app.orders; Duplicate entry '774411'
    for key 'orders.PRIMARY', Error_code: 1062; handler error
    HA_ERR_FOUND_DUPP_KEY; the event's source log ...". The value is the key's,
    which on a unique email column is an address.
  - Statement-based events: "Error 'Duplicate entry '774412' for key
    'orders.PRIMARY'' on query. Default database: ''. Query: 'INSERT INTO
    yc_app.orders (...) VALUES (774412, 2, 'yc-src-secret')'". The server
    quotes the message and the statement without escaping their quotes, so the
    SQL lexer paired them wrongly and **wrote `yc-src-secret` as it was**, in
    both columns of every sample.

  Fixed: these columns have a redactor of their own (`redactServerMessage`).
  Everything from a message's first quote to its last is replaced, to its end
  with an odd count; an apostrophe between two letters ("the event's") is not a
  quote. A message whose only quoted texts are GTIDs or ANONYMOUS is kept
  whole. A statement-based failure's `Query: '...'` is read as SQL, which there
  is properly escaped, and keeps its statement.
  `TestReplicaErrorsKeepTheirDiagnosticsAndLoseTheirValues` stages all three.

## Log permissions and the same-host yes (`perms` profile, 8.4.13)

`my84-perms` writes `/var/lib/mysql/error.log` where a real installation does,
`-rw-r----- mysql:mysql`, with no bind mount.

- **From the host the file is not there**, which is not the question the
  profile asks: `log_access=none`, `reason=unreadable` either way. Added:
  `my_metadata.txt`'s `log_open_error` says `not_found` or `permission_denied`.
  `TestPermsFromTheHostTheLogIsNotThere` asserts `not_found`.
- **Beside the server it is refused.** `scripts/mysql-perms-check.sh` builds this
  package's tests for Linux and runs `TestPermsBesideTheServer` in a container
  on `my84-perms`'s network with its data directory mounted read-only, as uid
  1000: `log_open_error,permission_denied`. `my_deadlocks.txt` says
  `reason=log_unreadable` and names its fallback, with no `matched=`; a deadlock
  forced during the window is in `my_buffer_pool.txt`'s LATEST DETECTED
  DEADLOCK with its row values replaced.
- **The same-host yes failed, with a no.** Sharing the server's process table
  (`--pid=container:`), the agent saw `mysqld` and said
  `agent_on_db_host=no reason=start_time_mismatch`, which would also overrule a
  declared `agentOnDbHost: true`. The image's entrypoint is a shell, pid 1,
  that initialises the data directory and then execs `mysqld`; exec keeps the
  pid and the start. Measured after the first boot: pid 1 started 6.6s before
  the server's `now - Uptime`, and `mysqld`'s first threads (InnoDB's I/O
  threads) 0.4s after it. A container initialised once and never restarted
  keeps that gap for life. Fixed: a process older than the server whose
  earliest thread but the first started within 2s of it is the server
  (`agent_on_db_host_by=thread_start_time`), read from `/proc/<pid>/task`.
  After a restart the entrypoint is quick (0.5s), and the process start alone
  answers. Without the shared process table: `unknown`, `reason=container`.

## MariaDB (`mariadb` profile, 10.11.6)

Out of scope (plan §1), so the question was only whether a run says what it
reached. `TestMariaDBIsNamedAndItsGapsRecorded` and
`TestMariaDBSessionIsBoundedUnderItsOwnNames` skip when the profile is not up.

- **Every file failed to connect.** The session's opening SET names
  `max_execution_time`, which MariaDB has never had (its bound is
  `max_statement_time`, in seconds), and `transaction_read_only`, which 10.11
  calls `tx_read_only`. Error 1193 failed the login, so `identify` never ran and
  every file read `connect_error="Error 1193 ... 'max_execution_time'"`:
  nothing said MariaDB. Fixed: the server's greeting, its first packet, carries
  its version (`5.5.5-10.11.6-MariaDB-1:10.11.6+maria~ubu2204`) in the clear
  before TLS and the login, and the agent now records it on every dial. When
  the SET fails with 1193 and the greeting names MariaDB, the login is made once
  more with MariaDB's names for the same bounds; `my_metadata.txt` then reports
  `mysql_variant=mariadb`. A refused login is not retried.
- **The image starts with TLS off**, so an omitted `tls:` fails every file with
  "TLS requested but server does not support TLS": right (the agent never falls
  back to plaintext), and true of any server without TLS. The tests connect in
  plaintext.
- **`performance_schema` is off by default on MariaDB**; the profile turns it
  on.
- **A standalone MariaDB reads as Galera.** It always reports the base
  `wsrep_*` status variables (`wsrep_cluster_size=0`,
  `wsrep_cluster_status=Disconnected`, `wsrep_connected=OFF`, `wsrep_ready=OFF`)
  with no provider loaded, so the spec's rule that any row confirms Galera
  makes `my_replication.txt` say `mechanism=galera`. Only those four came back;
  `wsrep_local_state_comment`, `wsrep_flow_control_paused` and
  `wsrep_local_recv_queue_avg` need a provider. Not fixed.
- **Version gates read MariaDB's numbers as MySQL's.** 10.11 compares above
  8.2, so `my_replication.txt` sends `SHOW BINARY LOG STATUS` (1064 on MariaDB)
  and `my_metadata.txt` says `has_statement_memory_tracking,true` with no such
  column. Not fixed.
- `information_schema.enabled_roles` has no `ROLE_HOST`, so the privilege read
  fails (`privileges_error`) and the `has_*` privilege rows are empty.
- MariaDB checks the privilege before the table: a role without the grant gets
  1142 for a table that does not exist, so `yc_restricted`'s files say
  `reason=permission_denied` where root's say 1146. `SHOW REPLICA STATUS` needs
  `SLAVE MONITOR` (or `SUPER`), which `yc_monitor`'s `REPLICATION CLIENT` does
  not give on MariaDB.

## Other findings

- **`EXPLAIN` of an `UPDATE` or `DELETE` fails in a read-only session**: error
  1792, "Cannot execute statement in a READ ONLY transaction". The session
  keeps `transaction_read_only=1`, and `my_explain.txt` skips write statements
  with `reason=read_only_session`.
- **9.x writes a second JSON plan format, and it carries the values
  elsewhere.** 9.7.4's `EXPLAIN FORMAT=JSON` is `json_schema_version` 2.0: the
  whole rewritten statement in `query`, and each step's expression in
  `operation` ("Filter: (orders.`status` = 'pending')") and `lookup_condition`
  ("customer_id = 7"). The first version of the plan redaction replaced values
  only under a list of condition keys and left these in place; the matrix
  caught it. Plan redaction is now deny-by-default: every string has its
  values replaced unless its key holds only names, structure or costs.
- **A prepared statement's sample keeps its `?` markers**, and EXPLAIN of it
  fails with 1064, which spent a plan on nothing. Such a sample is skipped as
  `reason=placeholder`.
- **EXPLAIN of a `FOR UPDATE` does not wait on a locked row**: it returned at
  once with the row held. The session's `innodb_lock_wait_timeout` is still set
  to 2s, against the server's 50s default, as a bound on what is not known.
- **9.7.4 reports `gtid_mode=ON`** with no option setting it.
- **A statement that failed has no sample.** `query_sample_text` is empty and
  `query_sample_seen` is `0000-00-00 00:00:00.000000` for a digest whose every
  run failed (the 3024 test's `SELECT SLEEP`).
- **`schema_name` is empty** for statements run with no default database. The
  digest key is still `(schema_name, digest)`.
- 8.0.46 logs a `[Warning] [MY-013360]` about `sha256_password`'s deprecation
  on every login by the image's own `mysql` client. Whether the agent's logins
  add the same line was not separated out.
- The two compose files shared one project name, the directory's, so each
  file's `up` and `down` saw the other's containers as orphans.
  `compose.mysql.yaml` now names its project `yc-mysql-matrix`.

## Goldens

The byte-for-byte contract for every file the package writes, as a real server
answered. `golden_test.go` replays a recording through the collectors with a
scripted clock - one window per connection, so each file's ticks come in one
order - and compares each file with its golden. No server is needed.

- `my84_root.json` - every statement of a 12s run against `my84` (8.4.13) as
  root, its columns and rows as the server returned them, recorded while
  `TestMatrixScenarios`' cases were staged and a deadlock forced: a row-lock
  waiter and a queued metadata lock, a held prepared statement with a literal,
  a statement past `max_execution_time`, a slow application SELECT and its plan,
  scans with no index.
- `my84_restricted.json` - the same run as `yc_restricted`, `USAGE` only.
- `my_<file>_full.txt` (16) and `my_<file>_least_privilege.txt` (16) - the
  replay of each.
- `my_metadata_connect_failure.txt`, `my_sessions_connect_failure.txt` - a run
  refused a connection slot; every other file's failure is the same envelope.

Two values in a recording are replaced as it is written, so a replay reads the
same on every machine: the client address the server saw (`192.0.2.10`, a
documentation address) and the matrix's host log directory
(`/yc-recording/logs/`, which exists nowhere). The error log is therefore not
found on replay, and `my_deadlocks.txt` shows the fallback form; the log tail's
own tests cover reading one. The same-host probe sees no server process.

To re-record, with the matrix up:

    YC_MY_RECORD=1 go test -tags mysqlintegration -run TestRecordGoldens ./internal/capture/mysql/
    YC_UPDATE_GOLDEN=1 go test -run TestGolden ./internal/capture/mysql/

and read the diff: a golden is only as right as what it was checked for.
