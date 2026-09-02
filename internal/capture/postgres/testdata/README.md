# Postgres artifact golden fixtures

The `.txt` files here are the byte-for-byte contract for the artifacts this
package writes, one per regime.
`pg_matrix_init.sql` is not a fixture — it is the role and extension setup
`compose.pg.yaml` mounts into each container for the opt-in version matrix
(`-tags pgintegration`), and it lives here because it is test input the package
owns.

The goldens:

- `pg_metadata_full.txt` — a complete capture: the preamble, the target block,
  the server block, the tablespace block, and the closing block. The split is
  the seam the capture already had — what was configured is knowable before
  the network, what the server said is not — so the block a reader can rely on
  is the one that is always there. **The tablespace block (2026-09-02) is the
  file's one tabular body**, `spcname,location`, one row per tablespace with
  storage of its own: a tablespace's name is data, and a key,value row would
  have made it a key. `pg_default` and `pg_global` live in the data directory
  and are not listed — `data_directory` in the server block is their location.
  The block is additive under a new `source=`; the four blocks before it are
  byte-identical to what they were, which is why `v` stays at 1 (see
  *Versioning*). PostgreSQL 17 deliberately: `pg_monitor` was not granted
  EXECUTE on `pg_current_logfile()` until 17, so a 14–16 fixture showing
  `has_pg_monitor_role,true` next to `log_access,direct` would depict a
  deployment that needs an extra manual grant.
  **Five of the settings rows exist to say what a zero in `pg_slow_queries.txt`
  means**, and they are populated here rather than empty for the same
  fixture-honesty reason: this cluster carries `pg_stat_statements` in
  `shared_preload_libraries`, and the extension's four GUCs exist in
  `pg_settings` exactly while the library is loaded, so empty cells beside a
  loaded library would depict a state the server cannot produce. The values are
  `pg_settings.setting`'s raw internal units — `5000`, `off`, `top` — never
  `SHOW`'s rendered forms. `track_io_timing,off` and
  `pg_stat_statements.track_planning,off` are the server's defaults, and they
  are why whole columns of `pg_slow_queries.txt` read `0` on an ordinary
  cluster: not "no I/O waiting" and not "planning is free", but *not measured*.
  **And `settings_unavailable` has changed meaning as a result.** Before this
  slice every captured setting was a core GUC, so a non-empty value meant a
  denial or a server too old to know the name. Now a perfectly healthy cluster
  that never preloaded the library lists those four names there — which is
  useful, because it separates *the library is not loaded* from *the extension
  is not created in this database*, two causes of the same empty artifact, and
  it agrees with `pg_slow_queries.txt`'s own `library_loaded=` from the other
  side. A reader treating the field as an alarm on its own will now raise one.
- `pg_metadata_connect_failure.txt` — a run that never reached the server. The
  absence of the server block is the discriminator, and `connect_error=` in the
  closing block's header says why. There is no `log_access` row: with no
  connection it would be `unknown` by construction, and the closing block says
  the same thing about the capture rather than about the server.
- `pg_bloat_full.txt` — a complete sampled capture: the preamble, two sample
  blocks, and the closing block that says both were written.
- `pg_bloat_connect_failure.txt` — the sampled equivalent of the above. Two
  lines, and the file exists at all because the preamble is written and synced
  before the connection is attempted.
- `pg_bloat_sample_error.txt` — one sample's statement timed out. The window
  writes the stub block carrying `sample_error=`, the window does not stop, and
  the closing block says `status=partial`.
- `pg_bloat_empty_db.txt` — a complete capture of a database with no user
  tables. A column header with no rows: captured and found nothing, which is a
  different shape from could not be captured.
- `pg_index_usage_full.txt` — the first artifact born under spec v1.2, and the
  first written into the bundle without being uploaded: `dt=pgIndexUsage` is
  proposed to the server team, not assigned. Four indexes on
  `pg_bloat_full.txt`'s two tables, kept consistent with that fixture: each
  table's two `idx_scan` counts sum to the table's there on both samples, and
  each table's two sizes sum to its `index_size_bytes`. `orders_status_idx` is
  the finding the artifact exists for — `idx_scan` stays `0` across the window,
  and `0` is a reading where an empty cell would mean not read. The spec's seven
  columns exactly and no schema or table name: `relid` is the join key into
  `pg_bloat.txt`, where those live.
- `pg_index_usage_connect_failure.txt`, `pg_index_usage_sample_error.txt` and
  `pg_index_usage_empty_db.txt` — the three regimes above, on the same clock as
  their `pg_bloat_*` counterparts.
- `pg_tablespaces_full.txt` — the spec's two columns, every tablespace every
  sample, `scope=cluster`. Three rows: the fixture cluster's one tablespace with
  storage of its own (the one `pg_metadata_full.txt`'s tablespace block
  locates), and the two every cluster has — `pg_default`, which holds every
  database and is the row that moves between the samples, and `pg_global`, the
  shared catalogues. Also held back from upload: `dt=pgTablespaces` is
  proposed, not assigned.
- `pg_tablespaces_least_privilege.txt` — a LOGIN-only role. `pg_tablespace_size`
  refuses a tablespace the role holds no CREATE on unless it is the database's
  own default tablespace or the role has `pg_read_all_stats`, and an error in a
  select list aborts the whole statement — so the statement guards the call
  with the server's own rule, and a refusal is an **empty cell, never a `0`**,
  counted on the header as `sizes_unread=2`. `pg_default` is read because it is
  this database's default; the other two are not. The sample is complete: the
  guard is what keeps a refusal from costing the whole artifact.
- `pg_tablespaces_connect_failure.txt` and `pg_tablespaces_sample_error.txt` —
  the two regimes, on the same clock as the `pg_bloat_*` counterparts.
- `pg_health_full.txt` — a complete interval capture, on a 30s window so three
  samples fit on a page; the default 120s window is the same shape with twelve.
  `pg_stat_database` is read **unfiltered**, so the block carries every database
  in the cluster plus the `datid=0` shared-objects row, whose `datname` is NULL
  because it accounts for shared relations rather than a database.
  `scope=cluster`, and `db=`/`dbid=` mean *connected through*, not *about*.
- `pg_health_connect_failure.txt` — two lines, as above.
- `pg_health_sample_error.txt` — sample 2's statement timed out. Sample 3 is
  still taken: a failed sample never ends the window.
- `pg_health_no_sessions_fatal.txt` — a server predating `sessions_fatal`
  (PostgreSQL 13 and older). The sample is retried without the column, the
  header carries `sessions_fatal=unavailable` with the quoted `reason=`, and
  the column stays in the body with every cell empty — a key present in one
  block of a shape is present in every block of that shape. `status=complete`:
  ten of eleven columns is a captured sample, not a failed one.
- `pg_capacity_pg17.txt` — a complete capture against PostgreSQL 17 or 18:
  **four sample blocks for two samples**, which is the first artifact where
  those are different numbers. The checkpoint block is written on both samples; the
  connection and WAL blocks are gauges — what exists as the window closes, not
  what happened during it — so they are written once, on the closing sample.
  `views=pg_stat_checkpointer,pg_stat_bgwriter` because 17 moved three of the
  five counters into a new view; `buffers_backend` is empty because that column
  was removed outright, and empty rather than `0` because `0` would mean
  backends wrote no buffers. The two reset clocks carry different values: the
  two views reset independently from 17 on, so one column would leave the
  other's counter with an undetectable reset.
- `pg_capacity_pre17.txt` — the same capture against 14–16, and the pair is the
  contract. **Exactly two structural differences are permitted**: `views=`, and
  a populated `buffers_backend`. Every column header is identical, the two reset
  clocks are the same value read twice, and anything else moving means the
  normalisation is incomplete.
- `pg_capacity_wal_denied.txt` — the least-privilege role. `pg_ls_waldir()`
  needs `pg_monitor` or superuser, so a role holding only `LOGIN` is denied: the
  WAL block is its header and its column header with `error=` saying why, the
  other two blocks are populated, and the artifact is `complete`. One refused
  read costs its own block, never the reads that succeeded beside it.
- `pg_capacity_connect_failure.txt` — two lines, as above.
- `pg_replication_full.txt` — a complete interval capture of a primary, on a 30s
  window so three samples fit on a page. **Two blocks per sample**: the
  connected WAL senders and the replication slots, in that order, sharing one
  `sample=` and one `ts=`. One replica and two slots — a physical slot held by
  the replica whose `active_pid` equals the `pid` in the block above it, and an
  abandoned logical slot with no consumer, which is the WAL-exhaustion incident.
  The slots are ordered by `slot_name`, so the logical one leads. `safe_wal_size`
  is populated, which means this fixture's cluster has `max_slot_wal_keep_size`
  set: at its `-1` default the column is NULL for **every** slot in the cluster,
  because it is a cluster GUC rather than a per-slot property, so a fixture
  showing one populated cell beside an empty one would depict something the
  server cannot produce.
  The block carries **21 columns on every supported version**, the last six read
  through `to_jsonb(s) ->> '...'` because 16, 17 and 18 each added some of them.
  `optional_columns=` in the header is which of the six the *server* has, read
  from `pg_attribute` in the same statement — not which are populated:
  `conflicting` is empty on the physical slot because it does not apply, and
  listed in the header because PostgreSQL 18 has the column. That distinction is
  the key's whole job, since an empty cell alone cannot separate *the server
  does not have this column* from *this column does not apply here*.
- `pg_replication_pre16.txt` — the same capture against 14 or 15, and the pair is
  the contract. **Exactly one structural difference is permitted**: the absent
  `optional_columns=` key. The column header is identical and the last six cells
  are empty, because the extraction yields NULL on a server without the column
  rather than raising, which is what lets one statement cover all five versions.
  The key is *absent* rather than empty: `string_agg` over no matching rows is
  NULL, and a NULL header value is never written.
- `pg_replication_none.txt` — a standalone server. Three samples of two
  header-only blocks, and `status=complete`. **No `optional_columns=` on any
  version**, because the presence set rides on the rows and there are none — and
  that is right, since with no rows there are no empty cells to disambiguate.
  This is what most captures look
  like, and it is the captured-and-found-nothing shape rather than a failure:
  "no replication is configured" is a finding, not an absence. The honest cost is
  ~26 lines of nothing on the majority of captures; writing the blocks only when
  non-empty would make an absent block ambiguous between *no replicas* and *the
  sample never ran*.
- `pg_replication_connect_failure.txt` — two lines, as above.
- `pg_sessions_full.txt` — a complete interval capture on a 6s window at a 2s
  cadence, so three samples fit on a page; the default 120s window is the same
  shape with sixty. **Two blocks per sample**, `pg_stat_activity` then `pg_locks`,
  sharing one `sample=` and one `ts=`. The cluster is a blocking chain: 1093 holds
  a row lock and sleeps inside its transaction, 1105 waits on 1093's transaction
  id and 1106 waits on the tuple lock 1105 now holds. The head of a chain is the
  session waiting on nothing — 1093 is `active` on `wait_event_type=Timeout`, not
  on a `Lock`. The three contending rows are byte-identical in every sample, which
  is what a stuck chain looks like: none of them has issued a new statement, so no
  clock moves, and `xact_start` is what turns "idle in transaction" from a state
  into a duration.

  **The chain is why both blocks live in one artifact, and it is readable off the
  file with two equi-joins and no `pg_blocking_pids()`.** 1105's `transactionid`
  row is `granted=false` with `waitstart` set, and the `granted=true` row for the
  same `789` belongs to 1093 — *1093 blocks 1105*. 1106's `tuple` row is
  `granted=false` on `(relation=16432, page=1, tuple=115)`, and the granted row
  for that tuple belongs to 1105 — *1105 blocks 1106*. A chain two deep already
  uses two different join keys, which is why `page` and `tuple` are captured
  rather than dropped as implementation detail: a capture without them would find
  one-deep chains and silently truncate deeper ones. `waitstart` is set on exactly
  the ungranted rows, so one sample carries a wait's duration — but `granted` is
  the marker and `waitstart` only the clock, since the server is allowed to record
  it slightly after a wait begins.

  Row 1116 is the capture's own backend, identifiable by
  `application_name=yc-360-postgres-capture` and captured like every other
  session: the requirements document asks for `WHERE pid <> pg_backend_pid()`
  and this statement has no `WHERE` clause at all, so the row count agrees with
  `pg_capacity.txt`'s and the server drops the row if it wants to. `datid`,
  `usesysid`, `database` and `relation` are OIDs and the agent never resolves any
  of them to a name — relation OIDs are per-database and collide across a cluster,
  so resolving one from the wrong database is silently wrong rather than merely
  unhelpful. The file shows what that costs: of the relations locked here, only
  `16432` is a user table the bundle can name from `pg_bloat.txt`; `16439` is its
  primary-key index and `12073` is `pg_locks` itself, and both stay digits.
- `pg_sessions_idle.txt` — no contention, which is the shape most captures
  produce and a finding rather than an absence: no `granted=false` row anywhere,
  every `waitstart` empty, and a locks block holding only the capture's own read.
  A backend idle between statements holds no locks at all — 1080 is `idle` with
  `xact_start` empty, which is exactly what separates it from the incident this
  artifact exists for, since `pg_stat_activity` still shows the last statement it
  ran. 1042 is the row `backend_type` exists to separate from an application
  connection: a background worker, whose `datid` and `datname` are empty because
  it belongs to no database, and whose `query` is an empty string rather than a
  NULL. Both render as an empty cell, which is the package's rule everywhere —
  empty means "not read", never "read as zero".
- `pg_sessions_least_privilege.txt` — **the file to read carefully.** The same
  cluster captured by a role holding nothing but `LOGIN`. Every row is present,
  the counts are right, `status=complete`, and there is no `error=` anywhere: a
  least-privilege capture of this artifact is *silent*. What degraded is per
  column, never per artifact. The identity columns survive — `pid`,
  `datid`/`datname`, `usesysid`/`usename`, `application_name`, and
  `backend_xid`/`backend_xmin` where the backend holds them — so the capture
  still says which database, which role and which application, and still names
  the session holding the transaction the others are queued behind. Thirteen
  cells are empty: the state, the wait detail, the clocks and the client
  identity. And `query` carries the literal string `<insufficient privilege>`
  rather than a NULL — the first column in the feature where the server hands
  back a sentence instead of an absence, and the one place the artifact's
  empty-means-not-read rule does not apply. A reader rendering it verbatim
  shows a cluster of sessions all running a statement by that name. The agent
  captures it as it arrived and never matches on it; `pg_metadata.txt`'s
  `has_pg_read_all_stats` is what tells a masked capture from an unmasked one.
  The capture's own row is unmasked, because a role always sees the sessions it
  owns — which is part of why the file looks complete.

  **And the `pg_locks` block below it is byte-identical to `pg_sessions_full.txt`'s**,
  because that view needs no grant at all. The pair is the contract: the two
  blocks of this artifact degrade in opposite directions, and at the privilege
  floor the capture still recovers the whole shape of the chain — 1093 holds
  `789`, 1105 is queued on it since `14:32:03.144`, 1106 is queued behind 1105 for
  the tuple — while being unable to quote a single statement. A report that
  suppressed this artifact for being under-privileged would throw that away.
- `pg_sessions_query_text.txt` — the fixture the parse contract has been missing.
  Three sessions: one whose `query` carries embedded newlines, one whose `query`
  carries a line beginning with `#` — the exact shape of a block header — and one
  carrying commas and double quotes. What it pins: **every data row is exactly
  one physical line, and no line but a block header begins with `#`.** The first
  holds because `singleLine` replaces every line break before the row is written,
  so `encoding/csv` never emits a multi-line record. The second holds because
  `pid` is the first column and is an integer on every row, which is why the
  column order is a decision rather than an aesthetic. A line-oriented parser and
  a record-aware parser therefore read this file identically. One divergence is
  visible in the first row and is deliberate: the agent collapses *line breaks*
  only, so the query's internal runs of spaces survive. That is a smaller
  mutation than "collapse whitespace runs", and it is the one that buys the parse
  property without further rewriting what the application submitted.
- `pg_slow_queries_full.txt` — a complete start-and-end capture against an
  extension-1.12 server. **Two blocks per sample and 37 columns**, the widest
  block in the feature, where the requirements document names eight. Read the
  first and last rows of the two `pg_stat_statements` blocks together and the
  artifact's whole purpose is on the page: `128740 − 128400 = 340` calls, and
  `(9891810.5 − 9820410.5) / 340 = 210.0` ms — the report's headline number,
  computed server-side from two blocks and nothing else. The agent joins
  nothing.
  **The `pg_stat_statements_info` block is what says that arithmetic means
  anything, and the requirements document does not ask for it.** `stats_reset`
  reads the same value in both samples here, which is the file saying no counter
  reset happened inside the window; had it moved, every delta above would be
  `end − start` across a reset — a large negative number, or worse, a small
  positive one that looks entirely plausible, with no in-band signal anywhere in
  the counters. `dealloc` is `0`; had it risen, entries were evicted because
  `pg_stat_statements.max` was exceeded, and eviction plus re-insertion restarts
  a `queryid`'s counters from zero, so its delta reads as a query that got
  dramatically faster. Neither is detectable from the 37 columns beside them.
  **The block leads sample 1 and closes sample 2**, and that is a decision rather
  than a layout: with a fixed statements-then-info order at both ends, a full
  reset landing between the opening statements read and its info read would
  invalidate the whole file while leaving the two `stats_reset` values equal.
  Read outermost, the two readings enclose every other read in the window.
  Within one `sample=` the block order is parser-neutral — it dispatches on
  `source=` — and both blocks carry the sample's single clock read as `ts=`.
  What the block cannot see is a **targeted** `pg_stat_statements_reset(userid,
  dbid, queryid)`: verified live on 18, it moves neither value. On extension
  1.11+ the per-row `stats_since` is the counter-signal; below 1.11 a targeted
  reset is undetectable, which is a documented limitation rather than a gap the
  agent can close.
  **The row order is by `queryid` ascending and it is not the readable order.**
  That is deliberate: the block is ordered on identity and never on a statistic,
  because a top-N taken independently at each endpoint can select two different
  sets and leave a query with no baseline to delta against. A fixture in a
  friendlier order would depict something the statement cannot return.
  **`blk_read_time` and `blk_write_time` are empty while `shared_blk_read_time`
  and `shared_blk_write_time` are `0`, in the same row.** Extension 1.11 renamed
  that pair *and re-scoped it* — before 1.11 it counted shared and local block
  I/O together — so they are two output columns and never both populated.
  Empty means the server does not have the column; `0` means it has it and
  `track_io_timing` is off. `optional_columns=` says which of the eleven the
  server had, and `pg_metadata.txt`'s `track_io_timing` says why the ones it had
  read zero. The same reading applies to the four plan columns, which are `0` on
  every row because `pg_stat_statements.track_planning` is off by default.
  **The fourth row of sample 2 is the agent's own opening read**, `calls=1`,
  under the capture role's `userid`. It is the only genuinely *new* entry the
  capture adds between the endpoints — every other collector's statements are
  entries in both samples, and their deltas are the window's capture cost
  itemised. This view has no `application_name` column, so `userid` and the
  statement text are the only handles a receiver has for telling capture
  overhead from customer workload.
- `pg_slow_queries_pre17.txt` — the same capture against an extension-1.10
  server, and the pair is the contract. **Exactly three structural differences
  are permitted**: the `blk_*_time` pair populated where the other file leaves
  it empty and the four 1.11 columns empty where the other populates them, both
  `*_since` columns empty, and a different `optional_columns=`. The column
  header is byte-identical, which is what one statement covering extension 1.8
  through 1.12 buys — the eleven varying columns are read out of a `to_jsonb` of
  the row, where a version without the column yields NULL rather than raising
  42703. Note which servers this is: 1.10 ships on PostgreSQL **15 and 16**,
  1.9 on 14, 1.11 on 17 and 1.12 on 18 — two server versions sharing one
  extension version, which is why `server_version_num` cannot even name this
  axis, let alone select on it.
- `pg_slow_queries_least_privilege.txt` — the file to read carefully, and the
  inverse of `pg_sessions_least_privilege.txt`. Every row is present, every
  counter is exact, `statements_total` is right, there is no `error=` anywhere
  and the artifact is `complete` — **and three of the four rows have no key.**
  A role without `pg_read_all_stats` reads `queryid` NULL and `query` as the
  `<insufficient privilege>` sentinel on every row it does not own, while
  `calls`, `total_exec_time` and the block counters stay correct: measured as
  `yc_restricted`, 277 of 319 rows on 18 and 124 of 152 on 14. Those rows cannot
  be merged against the other sample, told apart from each other, or joined to
  `pg_sessions.txt`'s `query_id`. In `pg_stat_activity` the identity survives
  masking and the detail is lost, so a least-privilege capture still shapes the
  blocking chain; here the detail survives and the identity does not.
  **The one surviving row leads, and that is the sort key working.** A role
  always sees the statements it executed itself, so the agent's own read keeps
  its `queryid` — and `ORDER BY … ASC` puts NULLs last, so the attributable rows
  sort first and a cap that binds at this privilege level sheds the
  unattributable ones first. Nothing in this file says the capture was degraded.
  `pg_metadata.txt`'s `has_pg_read_all_stats` does, and the count of empty lead
  cells does; that is the whole of the discriminator.
- `pg_slow_queries_query_text.txt` — this artifact's half of the parse contract,
  and it has one case `pg_sessions_query_text.txt` does not. Six rows: a query
  with embedded newlines, one carrying a line that begins with `#`, one with
  commas and double quotes, one multi-byte, one whose `query` is **NULL** — the
  shape the extension's documentation permits when its external query-text file
  has been discarded, where the row costs a cell rather than the block — and one
  masked row. What it pins is the same claim: **every data row is exactly one
  physical line, and no line but a block header begins with `#`.**
  **The second half of that claim holds for a different reason here, which is
  the thing a reviewer should check rather than assume.** In `pg_sessions.txt`
  the argument was that `pid` leads and is an integer on every row. Here the
  lead cell is `queryid`, which is *empty* on every masked row — so a data line
  can begin with a comma, and the last row of this file does. A line beginning
  with `,` is not a line beginning with `#`, so the contract holds; but the
  sessions argument does not transfer unchanged, and the comment on
  `statementColumnSpecs` says so.
- `pg_slow_queries_extension_absent.txt` — a complete two-sample capture of a
  database where `CREATE EXTENSION pg_stat_statements` was never run. Ten lines,
  not one data row, and **no `error=` anywhere**: `pg_stat_statements` holds
  cluster-wide statistics but its view exists only in the databases the
  extension was created in, so a capture pointed at `postgres` while the
  extension lives in `orders_db` has nothing to read — and that is a finding
  about the configuration, not a read that failed. `reason=extension_absent`
  carries it, on all four blocks, because one cause has to read as one cause;
  `library_loaded=true` beside it is the second half of the diagnosis, saying
  the cluster's problem is `CREATE EXTENSION` and not
  `shared_preload_libraries`. Three other reasons share the shape and are the
  fixtures' business rather than a golden's: `not_in_search_path` (with
  `schema_usage=` separating a path problem from a USAGE denial),
  `extension_too_old`, and `library_not_loaded`.
  **The block order is the other thing this file pins.** `pg_stat_statements_info`
  leads sample 1 and closes sample 2, so the two `stats_reset` readings enclose
  everything between the endpoints.
  **And it is the golden that does not move when the info block gains its
  read** — its four blocks were already header-only under the shared `reason=`,
  which is that rule visibly working. The one state where the two blocks of a
  sample carry *different* reasons is `reason=view_absent` on the info block,
  and it is exactly one extension version wide: measured on 18 by installing
  each in turn, 1.7 has neither `total_exec_time` nor the info view, **1.8 has
  the first and not the second**, and 1.9 has both. So a healthy extension at
  the floor reads its statements normally beside an info block that says the
  view has not been written yet — and `view_absent` never appears beside one of
  the four absences, where it would read as a second, unrelated problem.
- `pg_deadlocks_full.txt` — **the first artifact here that is not CSV**, and the
  only shape in the bundle whose body is opaque bytes. A 30-second window on a
  cluster logging to `stderr`, with one real deadlock: sample 1 seeks to EOF and
  matches nothing, sample 2 reads unrelated traffic, the event lands in sample 3
  — the first read taken *after* it was written, which is the sense in which the
  ten-second interval is a latency and not a loss — and the drain closes the
  final interval `Every`'s offsets leave open.
  **`bytes=` is the parsing contract and nothing else is.** Read the header line,
  read exactly that many bytes, expect the next header line; where `bytes=` is
  absent, as on the preamble and the closing block, there is no body. Never scan
  for a terminator: a first log line begins with the expansion of
  `log_line_prefix`, which the customer configures and which may legally begin
  with `#`. `bytes=` counts **bytes, not runes**, and the body may not be valid
  UTF-8 — this is the one artifact whose content the agent does not encode.
  The body always ends with a newline, which is a convenience for a pager and
  for `grep` and is *not* the contract.
  **The event's bytes are measured**, from a `postgres:18` container on
  2026-08-15, and two things in them are not in requirements §2.3: `ERROR:` is
  followed by **two** spaces, and the report continues past `DETAIL:` into
  `HINT:`, `CONTEXT:` and `STATEMENT:`. A matcher written from the document
  matches nothing, on every server, forever, while reporting `status=complete`.
  The `log_path=` here is relative because the fixture's data directory is the
  test's working directory, which is what makes the golden reproducible on every
  machine; a real server reports an absolute one.
- `pg_deadlocks_csvlog.txt` — the same event as a CSV *record* spanning four
  physical lines, its `DETAIL` inside a quoted field with real newlines in it.
  This is the fixture that shows why the body is length-delimited: in `csvlog` a
  body line can begin with anything at all, `#` included. `matched_by=sqlstate`,
  because the format carries `40P01` in a dedicated column and a five-character
  code is exact where a message is translatable.
- `pg_deadlocks_jsonlog.txt` — the same event again, as one JSON line. The only
  format with no boundary problem at all: one line, one event.
- `pg_deadlocks_remote.txt` — Mode R, and **the file the "not observable"
  rendering is built against.** Twelve header-only blocks, each
  `reason=unreadable log_access=none`, and **no `matched=` key anywhere in
  it**. The missing key is the design: `matched=0` is a measurement — the log was
  read and held no event — where a `reason=` is an absence of measurement.
  Writing `matched=0` beside a reason would put a number in the file that a
  receiver can sum, average or render as a green tick. `status=complete` is
  honest: every scheduled sample was written, and the artifact's content is the
  reason it is empty. Four causes produce this shape and each is written rather
  than summarised — `collector_off`, `unresolved`, `unreadable`, `settings_unread`.
- `pg_timeouts_full.txt` — all three timeout types in one window, at their
  measured line counts: the statement timeout is two lines, the lock timeout is
  three (its `CONTEXT:` names the relation and the tuple), and **the
  idle-in-transaction `FATAL` is one**. That last one is the fixture that would
  have prevented a bug in the requirements document, which shows a `STATEMENT:`
  line PostgreSQL does not write and cannot: the timeout fires precisely because
  the backend is running no statement. A parser implementing "the line plus the
  following `STATEMENT:` line" either drops the event or attaches the *next*
  event's statement to it — inventing a statement for the timeout a DBA uses to
  find which application leaked a transaction.
- `pg_timeouts_unreadable.txt` — the outcome an operator hits first, and the one
  artifact in the feature with no fallback of any kind. `pg_health.txt`'s
  `pg_stat_database.deadlocks` counter is a substitute for deadlocks in Mode R;
  **nothing anywhere counts timeouts.**

## Reader requirements

They apply to every consumer, including the server-side parser. `parseArtifact`
in `writer_test.go` is the reference implementation, and
`TestBlockHeaderIsNotACSVRecord` and `TestMetadataArtifactParsesByTheDocumentedRule`
pin the rule below against the driver text that motivates it.

- **Split the block headers off by their leading `#` before parsing anything as
  CSV.** A block header is **not** a CSV record and must never be handed to a
  CSV parser. `headerValue` quotes any value containing whitespace, which is
  every driver message, so a header carrying `connect_error=` or `reason=`
  presents as a bare `"` in a non-quoted field — a parse error, not a record.
  A value carrying a comma splits into several fields on top of that. This is
  not a corner case: it is what every refused connection writes, and a refused
  connection is what the artifact exists to record.
  The lines are unambiguous — the writer flattens every value onto one line, so
  one record is one line, and a line beginning with `#` is a block header and
  nothing else is.
- **Parse each block's body separately**, between its own header and the next.
  Every block that has a body opens it with its own column header, so a block is
  readable without the one before it — and a parser that concatenates the bodies
  has to guess which `key,value` line is a column header and which is data.
- Parse CSV **records**, not lines, within a body. Every body record has exactly
  two fields for `pg_metadata.txt`, and the artifact's own column count for a
  tabular one.
- Allow variable field counts anyway. In Go: `reader.FieldsPerRecord = -1`.
- Do **not** treat `#` as a comment character (in Go: leave `Comment` unset).
  That configuration parses "cleanly" and silently discards the block headers
  carrying `engine=`, `v=`, `format=` and `scope=`.
- **One file may carry more than one `source=`.** The window's own blocks name
  the artifact; a collector's blocks name what they read. `pg_health.txt`
  carries `pg_health` and `pg_stat_database`, `pg_bloat.txt` carries `pg_bloat`
  and `pg_stat_user_tables`, `pg_index_usage.txt` carries `pg_index_usage` and
  `pg_stat_user_indexes`, `pg_tablespaces.txt` carries `pg_tablespaces` and
  `pg_tablespace_size`, `pg_metadata.txt` carries four:
  `pg_metadata` for the preamble and the closing block, `pg_metadata_target`
  for what was configured, `pg_metadata_server` for what the server said, and
  `pg_metadata_tablespaces` for where its tablespaces live —
  `pg_capacity.txt` carries four: `pg_capacity`, `pg_checkpointer`,
  `pg_stat_activity_by_app` and `pg_ls_waldir` — `pg_replication.txt`
  carries three: `pg_replication`, `pg_stat_replication` and
  `pg_replication_slots` — and `pg_sessions.txt` three: `pg_sessions`,
  `pg_stat_activity` and `pg_locks`. **One `source=` is one shape**, which is why
  `pg_capacity.txt` reads the same view under `pg_stat_activity_by_app`: it
  carries counts per `(application_name, backend_type)` group where
  `pg_sessions.txt` carries a row per backend, and two shapes under one dispatch
  key would make the column header load-bearing for dispatch.
- **One sample may be more than one block, and `samples_expected` counts
  samples.** `pg_capacity.txt` writes four sample blocks for two samples — one
  on the opening sample and three on the closing one — and a reader that counted
  blocks would call that file incomplete. `pg_replication.txt` and
  `pg_sessions.txt` are the stronger case: both write two blocks on **every**
  sample, so their block count is never their sample count — and on the default
  window `pg_sessions.txt` writes 120 sample blocks for 60 samples. Group a
  collector's
  blocks into samples by `sample=`, which every one of them carries; the
  artifact's own `samples_expected` and `samples_written` are about samples and
  nothing else.
- **A block whose own read failed is still written**, with `error=` in its
  header and no rows under its column header. Within one sample the blocks fail
  independently: a `pg_capacity.txt` sample can carry two populated blocks and
  one that says why it is empty, and it is still a complete sample. The
  artifact-level stub — `sample_error=` on a block naming the artifact — is a
  different thing, and means the collector could not localise the failure at
  all.

### Block header keys

Split the header on unquoted whitespace into `k=v` tokens. `engine`, `source`,
`v`, `format` and `scope` always lead and `ts` always closes, so a block's
identity and its clock read are readable without parsing the middle.

- A key written with an empty value means "not read" — `dbid=` before a
  connection exists. It is not the same as the key being absent.
- Header keys, unlike body keys, may be **conditional**: `sizes=`, `reason=`,
  `queries_truncated=`, `error=` and `connect_error=` appear only when the thing
  they describe happened, and in each case absence is itself the value.
  `pg_sessions.txt`'s is the one to read as a distinction rather than a warning:
  its absence means every `query` cell is the server's own text, and its presence
  means the agent cut that many of them at its own 8192-rune cap, marking each
  with a trailing `...`. The server's own truncation, at
  `track_activity_query_size`, ends a statement mid-token with no marker at all —
  `pg_metadata.txt` records that limit so the two stay tellable apart.
  `pg_capacity.txt`'s
  connection block goes further and drops its three count keys when the read
  failed: `groups_total=0` would assert that the server has no connections,
  where the truth is that nobody could count them.
- An artifact's preamble carries `schedule=` (`start_end`, `every` or `once`)
  and `interval=` (`10s`, or empty on the other two). Both are written for every
  artifact on every schedule, so the preamble is one fixed key set:
  `samples_expected` does not explain itself without the cadence, and the
  cadence is the agent's constant rather than the server's.
- **Deltas are computed against each block's own `ts=`, never against
  `interval=`.** A sample runs late rather than being skipped, so two blocks
  with near-identical `ts=` mean the sampler was catching up. `interval=` is
  the nominal cadence, there to be compared against `ts=`.
- **`ts=` is the sample's clock read, not the block's.** Every block of one
  `sample=` carries the same value, taken before the sample's first statement
  ran — so in `pg_capacity.txt` the WAL block's `ts=` can precede its own read
  by as much as two statement timeouts. Equal `ts=` within one `sample=` is by
  construction and says nothing about the sampler catching up; the rule above
  applies across samples.

### Versioning

`v=` is the artifact format version. The rule, stated once so it need not be
inferred from a diff: **additive keys do not bump `v`; removed or re-formed
keys do.** A reader that tokenises `k=v` is not got wrong by a key it has never
seen, and is got wrong by one that has changed meaning or gone away. `v=1`
therefore survived `schedule=` and `interval=` arriving.

`v=1` also survived `pg_metadata.txt` becoming four blocks, and that one is an
exception rather than the rule: the block structure changed, and a reader
written against the one-block form **would** be got wrong. It stays at 1 only
because no such reader exists — confirmed with the server team on 2026-08-09,
in as many words: no parser has been written against `pg_metadata.txt` v=1.
`pg_metadata.txt` has never had a `fromAgentFileName` entry either, so no bundle
has ever been classified and nothing has ever read one.

**That exemption is spent.** The next time any one artifact's shape moves alone,
`v` belongs on `Artifact` rather than on the package: it is one constant today,
stamped into every block of every artifact, so bumping it would announce a break
in `pg_bloat.txt` and `pg_health.txt`, which have not changed shape.

The tablespace block added to `pg_metadata.txt` on 2026-09-02 is not that case,
and the distinction is worth stating once: it is a **fifth block under a new
`source=`**, and the four before it did not move — same keys, same order, same
bytes. A reader that dispatches on `source=` and skips one it does not know is
not got wrong; a reader that counts four blocks is, and the server team is asked
to confirm the `pgMeta` receiver is the former. Had any existing block changed
shape, this paragraph would be about moving `v` instead.

## Editing

The files are LF-terminated with a single trailing newline; `.gitattributes`
pins that against eol rewriting. `log_line_prefix`'s value ends in a
significant trailing space — `TestGoldenKeepsTrailingWhitespace` guards it
against trimming editors.

To change a fixture, change the writer or the samples in `writer_test.go`,
`bloat_test.go`, `indexusage_test.go`, `tablespaces_test.go`, `health_test.go`, `capacity_test.go`, `replication_test.go`,
`sessions_test.go`, `deadlocks_test.go` and `timeouts_test.go`, and argue the
resulting diff — never hand-edit these files.

The six `pg_deadlocks_*` and `pg_timeouts_*` goldens are written by a real
`Window` over a real log file in a temporary directory, so what is checked in is
the bytes the agent wrote rather than a transcription. The log bytes those tests
feed it are themselves measured from a running server — which is the whole
mitigation for a requirements document that describes a log line PostgreSQL does
not write.
