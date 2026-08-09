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
  `pg_stat_activity_by_app` and `pg_ls_waldir` — and `pg_replication.txt`
  carries three: `pg_replication`, `pg_stat_replication` and
  `pg_replication_slots`.
- **One sample may be more than one block, and `samples_expected` counts
  samples.** `pg_capacity.txt` writes four sample blocks for two samples — one
  on the opening sample and three on the closing one — and a reader that counted
  blocks would call that file incomplete. `pg_replication.txt` is the stronger
  case: it writes two blocks on **every** sample, so its block count is never
  its sample count. Group a collector's
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
  `error=` and `connect_error=` appear only when the thing they describe
  happened, and in each case absence is itself the value. `pg_capacity.txt`'s
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
`bloat_test.go`, `health_test.go`, `capacity_test.go` and
`replication_test.go`, and argue the resulting diff — never hand-edit these
files.
