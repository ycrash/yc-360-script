# MySQL capture matrix — spike results

`my_matrix_init.sql` and `my_replica_init.sql` are not fixtures: they are the
roles, tables and replication setup `compose.mysql.yaml` mounts into each
container for the opt-in matrix (`-tags mysqlintegration`), and they live here
because they are test input the package owns.

This file is step 1 of `mysql-capture-plan.md`'s order of work: what the spec's
queries do on real servers, recorded so the code that depends on them does not
rest on belief. Measured 2026-10-27.

## What was run

| Server | Image | Measured |
|---|---|---|
| `my80` | `mysql:8.0` → 8.0.46 | yes, all three roles |
| `my80old` | `mysql:8.0.27` (amd64, emulated on Apple Silicon) | yes, all three roles |
| `my84` | `mysql:8.4` → 8.4.13 | **no**: the Docker VM's disk filled before it could initialise |
| `my9` | `mysql:9` | **no**: image not pulled, same cause |
| `percona84` | `percona/percona-server:8.4` | **no**: image not pulled, same cause |

Everything below marked *8.0 only* has been seen on 8.0.46 and 8.0.27 and
nowhere else. `go test -tags mysqlintegration` with
`YC_MY_MATRIX_SERVERS=my80,my80old` passes; the other three servers' rows of
`TestMatrixWindow`, and `TestMatrixVerifiedTLS` (my84's certificate), have not
yet run.

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
- **MariaDB lacks the tables the spec relies on.** Not measured; the variant is
  detected from `VERSION()` and recorded.

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

## Capability flags (8.0 only)

| Flag | `my80` (8.0.46) | `my80old` (8.0.27) |
|---|---|---|
| `has_cpu_time_instrument_enabled` | false, `consumers_off` | false, `version_before_8.0.28` |
| `has_statement_memory_tracking` | true | false |
| `has_memory_instruments_enabled` | false (`memory/%=OFF`) | true |

- `--performance-schema-instrument=memory/%=OFF` leaves the 76
  `memory/performance_schema/%` instruments on: they cannot be switched off.
  `has_memory_instruments_enabled` counts every other `memory/%` instrument.
  With them off, the memory summaries read zeros, not NULLs.
- On 8.0.27 the `events_statements_cpu` consumer does not exist, and
  `sum_cpu_time`, `max_total_memory` and `max_controlled_memory` are not
  columns: `my_slow_queries.txt` selects `NULL` in their place and names them
  as `unavailable=`.
- The CPU-on path (my84, my9) is untested.

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
   STATUS` before. On 8.0.46 `SHOW MASTER STATUS` still works. 8.4's removal of
   it is not measured here.
6. **`my_nondefault_settings.txt`.** The joined query is written:
   `variables_info` LEFT JOIN `global_variables`, so a session-only variable
   keeps an empty value.
7. **`my_metadata.txt`'s flags.** All written; see the table above.
8. **Same-host detection.** MySQL is one process, so the test is the server's
   own start: a local `mysqld` or `mariadbd` whose start time is within 2s of
   `now - Uptime`. On this machine Docker runs the servers in a VM, and the
   verdict is `no reason=no_server_process`, which is right. The yes path is
   unit-tested only.

## Other findings

- **`EXPLAIN` of an `UPDATE` or `DELETE` fails in a read-only session**: error
  1792, "Cannot execute statement in a READ ONLY transaction". The session
  keeps `transaction_read_only=1`, and `my_explain.txt` skips write statements
  with `reason=read_only_session`.
- **A statement that failed has no sample.** `query_sample_text` is empty and
  `query_sample_seen` is `0000-00-00 00:00:00.000000` for a digest whose every
  run failed (the 3024 test's `SELECT SLEEP`).
- **`schema_name` is empty** for statements run with no default database. The
  digest key is still `(schema_name, digest)`.
- 8.0.46 logs a `[Warning] [MY-013360]` about `sha256_password`'s deprecation
  on every login by the image's own `mysql` client. Whether the agent's logins
  add the same line was not separated out.
- The Docker VM used for this spike was at 100% of its 59 GB disk, filled by
  other projects' images, volumes and build cache, which is what stopped
  `my84`, `my9` and `percona84`.
