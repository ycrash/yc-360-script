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
  deployment that needs an extra manual grant (`pg_metadata_impl_notes.txt`
  §2a).
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
  and `pg_stat_user_tables`, and `pg_metadata.txt` carries three:
  `pg_metadata` for the preamble and the closing block, `pg_metadata_target`
  for what was configured, and `pg_metadata_server` for what the server said.

### Block header keys

Split the header on unquoted whitespace into `k=v` tokens. `engine`, `source`,
`v`, `format` and `scope` always lead and `ts` always closes, so a block's
identity and its clock read are readable without parsing the middle.

- A key written with an empty value means "not read" — `dbid=` before a
  connection exists. It is not the same as the key being absent.
- Header keys, unlike body keys, may be **conditional**: `sizes=`, `reason=`
  and `connect_error=` appear only when the thing they describe happened, and
  in each case absence is itself the value.
- An artifact's preamble carries `schedule=` (`start_end`, `every` or `once`)
  and `interval=` (`10s`, or empty on the other two). Both are written for every
  artifact on every schedule, so the preamble is one fixed key set:
  `samples_expected` does not explain itself without the cadence, and the
  cadence is the agent's constant rather than the server's.
- **Deltas are computed against each block's own `ts=`, never against
  `interval=`.** A sample runs late rather than being skipped, so two blocks
  with near-identical `ts=` mean the sampler was catching up. `interval=` is
  the nominal cadence, there to be compared against `ts=`.

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
`bloat_test.go` and `health_test.go`, and argue the resulting diff — never
hand-edit these files.
