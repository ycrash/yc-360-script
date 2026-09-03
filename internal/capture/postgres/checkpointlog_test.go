package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// measuredCheckpoint is a completion line copied from the matrix's postgres:18
// container log on 2026-09-02, SLRU and lsn fields included: 18's shape.
const measuredCheckpoint = "2026-09-02 15:20:25.938 UTC [31] LOG:  checkpoint complete: wrote 20 buffers (0.1%), " +
	"wrote 1 SLRU buffers; 0 WAL file(s) added, 0 removed, 0 recycled; write=0.003 s, sync=0.006 s, " +
	"total=0.015 s; sync files=21, longest=0.005 s, average=0.001 s; distance=98 kB, estimate=1288 kB; " +
	"lsn=0/42D5018, redo lsn=0/42D4FC0\n"

// pre18Checkpoint is the pre-18 shape, without those fields.
const pre18Checkpoint = "2026-07-25 14:33:47.220 UTC [412] LOG:  checkpoint complete: wrote 1204 buffers (7.3%); " +
	"0 WAL file(s) added, 0 removed, 2 recycled; write=4.812 s, sync=1.940 s, total=6.901 s; sync files=18, " +
	"longest=0.612 s, average=0.108 s; distance=48210 kB, estimate=52104 kB\n"

// The lines around a completion line that must not be taken for one: the
// checkpoint's own starting line before it, and an unrelated entry after it,
// which the stderr boundary rule needs to prove the one-line event ended.
// unrelatedTraffic is not unrelated here - it carries a completion line of its
// own - which is why these goldens keep their own neighbours.
const (
	checkpointStarting = "2026-09-02 15:20:25.920 UTC [31] LOG:  checkpoint starting: immediate force wait\n"
	checkpointFollower = "2026-09-02 15:20:26.101 UTC [7301] LOG:  duration: 1200.522 ms  statement: SELECT count(*) FROM orders\n"
)

func TestCheckpointLogArtifact(t *testing.T) {
	artifact := NewCheckpointLog().Artifact()

	assert.Equal(t, "pg_checkpoint_log", artifact.Name)
	assert.Equal(t, "pg_checkpoint_log.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope)
	assert.Equal(t, formatText, artifact.Format)
	assert.Equal(t, Every(DefaultLogTailInterval), artifact.Schedule,
		"a log tail, not a sampled artifact: the poll bounds what a cancelled window loses")
	assert.Equal(t, LogDrainBudget, artifact.SampleBudget)

	var _ Collector = NewCheckpointLog()
	var _ Closing = NewCheckpointLog()
}

func TestCheckpointLogMatchesTheCompletionLineAlone(t *testing.T) {
	body, matched := matchBody(logFormatStderr, checkpointMatch,
		checkpointStarting+measuredCheckpoint+checkpointFollower)

	require.Equal(t, 1, matched)
	assert.Equal(t, measuredCheckpoint, body, "the completion line, verbatim, and nothing around it")
	assert.Equal(t, 1, strings.Count(body, "\n"), "one line: none of the multi-line handling applies")
}

func TestCheckpointLogDoesNotMatchTheStartingLine(t *testing.T) {
	_, matched := matchBody(logFormatStderr, checkpointMatch, checkpointStarting+checkpointFollower)

	assert.Zero(t, matched, "the starting line carries no numbers; the completion line is the finding")
}

func TestCheckpointLogEventEndsAtTheNextEntryFromTheSamePID(t *testing.T) {
	next := "2026-09-02 15:25:25.920 UTC [31] LOG:  checkpoint starting: time\n"

	body, matched := matchBody(logFormatStderr, checkpointMatch, measuredCheckpoint+next)

	require.Equal(t, 1, matched)
	assert.Equal(t, measuredCheckpoint, body,
		"the checkpointer's next line shares its PID, and is still a new entry")
}

func TestCheckpointLogSpecShapeMatchesToo(t *testing.T) {
	body, matched := matchBody(logFormatStderr, checkpointMatch, pre18Checkpoint+checkpointFollower)

	require.Equal(t, 1, matched)
	assert.Equal(t, pre18Checkpoint, body, "the pre-18 line, without the SLRU and lsn fields")
}

func TestCheckpointLogMatchesByMessageInTheStructuredFormats(t *testing.T) {
	complete := timeoutCSV("LOG", "00000", "checkpoint complete: wrote 20 buffers (0.1%)")

	body, matched := matchBody(logFormatCSV, checkpointMatch, unrelatedCSV+complete)
	require.Equal(t, 1, matched, "csvlog: 00000 names nothing, so the message decides")
	assert.Equal(t, complete, body)

	_, matched = matchBody(logFormatCSV, checkpointMatch, unrelatedCSV)
	assert.Zero(t, matched, "the starting line carries the same 00000 and is not matched")

	completeJSON := `{"timestamp":"2026-09-02 15:20:25.938 UTC","pid":31,"error_severity":"LOG",` +
		`"message":"checkpoint complete: wrote 20 buffers (0.1%)","backend_type":"checkpointer"}` + "\n"

	body, matched = matchBody(logFormatJSON, checkpointMatch, unrelatedJSON+completeJSON)
	require.Equal(t, 1, matched, "jsonlog, on the same terms")
	assert.Equal(t, completeJSON, body)
}

func TestCheckpointLogReportsMatchedByMessageInEveryFormat(t *testing.T) {
	for _, format := range []logFormat{logFormatStderr, logFormatCSV, logFormatJSON} {
		tail := newLogTail("pg_checkpoint_log", checkpointMatch)
		tail.source.format = format

		assert.Equal(t, matchedByMessage, tail.matchedBy(),
			"%s: every code the matcher accepts is paired with its message, so no code decided", format)
	}

	for name, match := range map[string]eventMatch{"deadlocks": deadlockMatch, "timeouts": timeoutMatch} {
		tail := newLogTail(name, match)
		tail.source.format = logFormatCSV

		assert.Equal(t, matchedBySQLState, tail.matchedBy(),
			"%s: a code stands alone for at least one event, and the siblings' headers do not move", name)

		tail.source.format = logFormatStderr
		assert.Equal(t, matchedByMessage, tail.matchedBy())
	}

	assert.True(t, checkpointMatch.messageDecides())
	assert.False(t, deadlockMatch.messageDecides())
	assert.False(t, timeoutMatch.messageDecides(), "two of its three codes are paired; the third stands alone")
	assert.True(t, explainMatch.messageDecides(), "the tail that set the LOG-line precedent")
}

func TestCheckpointLogTranslatedLineMatchesNothingAndBreaksNothing(t *testing.T) {
	translated := "2026-09-02 15:20:25.938 UTC [31] LOG:  Checkpoint komplett: 20 Puffer geschrieben (0.1%)\n"

	_, matched := matchBody(logFormatStderr, checkpointMatch, translated+checkpointFollower)

	assert.Zero(t, matched,
		"lc_messages translates the message and there is no code to fall back on: a clean "+
			"matched=0, never a mis-attributed line")
}

var checkpointGoldenWrites = []string{
	checkpointStarting + measuredCheckpoint + checkpointFollower,
	checkpointFollower,
	checkpointStarting + pre18Checkpoint + checkpointFollower,
}

func TestCheckpointLogGoldenFull(t *testing.T) {
	results := runLogGoldenWindow(t, NewCheckpointLog(), logFormatStderr,
		priorTraffic, checkpointGoldenWrites, 30*time.Second, logGoldenClock(t, 3))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 3, results[0].SamplesWritten, "the drain is not a sample")

	artifact := artifactText(t, results[0])

	assert.NotContains(t, artifact, "checkpoint starting:")
	assert.NotContains(t, artifact, "matched_by=sqlstate",
		"every block says the message decided, csvlog or not")

	assert.Equal(t, bloatGolden(t, "pg_checkpoint_log_full.txt"), artifact)
}

func TestCheckpointLogGoldenUnreadable(t *testing.T) {
	results := runRemoteGoldenWindow(t, NewCheckpointLog(), 30*time.Second, logGoldenClock(t, 3))

	require.Equal(t, StatusComplete, results[0].Status)

	artifact := artifactText(t, results[0])

	assert.NotContains(t, artifact, "matched=",
		"the counters in pg_capacity.txt say how many checkpoints ran; this file says what "+
			"each cost, and a zero here would claim none did")

	assert.Equal(t, bloatGolden(t, "pg_checkpoint_log_unreadable.txt"), artifact)
}
