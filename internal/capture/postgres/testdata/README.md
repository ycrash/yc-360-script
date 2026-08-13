# Postgres artifact golden fixtures

The `.txt` files here are the byte-for-byte contract for the artifacts this
package writes, one per regime.
`pg_matrix_init.sql` is not a fixture — it is the role and extension setup
`compose.pg.yaml` mounts into each container for the opt-in version matrix
(`-tags pgintegration`), and it lives here because it is test input the package
owns.

The goldens:

- `pg_metadata_full.txt` — a complete capture: the preamble, the target block,
  the server block, and the closing block. The split is the seam the capture
  already had — what was configured is knowable before the network, what the
  server said is not — so the block a reader can rely on is the one that is
  always there. PostgreSQL 17 deliberately: `pg_monitor` was not granted
  EXECUTE on `pg_current_logfile()` until 17, so a 14–16 fixture showing
  `has_pg_monitor_role,true` next to `capture_mode,pg-dbhost` would depict a
  deployment that needs an extra manual grant.
- `pg_metadata_connect_failure.txt` — a run that never reached the server. The
  absence of the server block is the discriminator, and `connect_error=` in the
  closing block's header says why. There is no `capture_mode` row: with no
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
  and `pg_stat_user_tables`, `pg_metadata.txt` carries three:
  `pg_metadata` for the preamble and the closing block, `pg_metadata_target`
  for what was configured, and `pg_metadata_server` for what the server said —
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

## Editing

The files are LF-terminated with a single trailing newline; `.gitattributes`
pins that against eol rewriting. `log_line_prefix`'s value ends in a
significant trailing space — `TestGoldenKeepsTrailingWhitespace` guards it
against trimming editors.

To change a fixture, change the writer or the samples in `writer_test.go`,
`bloat_test.go`, `health_test.go`, `capacity_test.go`, `replication_test.go` and
`sessions_test.go`, and argue the resulting diff — never hand-edit these files.
