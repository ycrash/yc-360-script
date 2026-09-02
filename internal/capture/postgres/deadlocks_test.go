package postgres

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const measuredDeadlockCSV = `2026-08-15 10:02:48.397 UTC,"postgres","postgres",112,"[local]",6a803945.70,1,"UPDATE",` +
	`2026-08-15 10:02:40.123 UTC,3/12,754,ERROR,40P01,"deadlock detected","Process 112 waits for ShareLock on transaction 754; blocked by process 105.
Process 105 waits for ShareLock on transaction 755; blocked by process 112.
Process 112: UPDATE yc_dl SET v=2 WHERE id=2;
Process 105: UPDATE yc_dl SET v=1 WHERE id=1;","See server log for query details.",,,` +
	`"while updating tuple (0,1) in relation ""yc_dl""","UPDATE yc_dl SET v=2 WHERE id=2;",,,"psql","client backend",,0
`

const measuredDeadlockJSON = `{"timestamp":"2026-08-15 10:02:48.397 UTC","user":"postgres","dbname":"postgres","pid":112,` +
	`"error_severity":"ERROR","state_code":"40P01","message":"deadlock detected",` +
	`"detail":"Process 112 waits for ShareLock on transaction 754; blocked by process 105.\nProcess 105 waits for ShareLock on transaction 755; blocked by process 112.",` +
	`"hint":"See server log for query details.","context":"while updating tuple (0,1) in relation \"yc_dl\"",` +
	`"statement":"UPDATE yc_dl SET v=2 WHERE id=2;","application_name":"psql","backend_type":"client backend","query_id":0}
`

const unrelatedCSV = `2026-08-15 10:02:50.000 UTC,"postgres","postgres",113,"[local]",6a803945.71,1,"idle",` +
	`2026-08-15 10:02:49.000 UTC,3/13,0,LOG,00000,"checkpoint starting: time",,,,,,,,,"psql","client backend",,0
`

const unrelatedJSON = `{"timestamp":"2026-08-15 10:02:50.000 UTC","pid":113,"error_severity":"LOG",` +
	`"state_code":"00000","message":"checkpoint starting: time","backend_type":"client backend"}
`

func matchOnce(format logFormat, m eventMatch, data string) (body, pending string, pendingIsEvent bool, matched int, read *tailRead) {
	read = &tailRead{}

	events, rawCarry, pendingIsEvent, matched := matchEvents([]byte(data), format, m, read)

	return string(bytes.Join(events, nil)), string(rawCarry), pendingIsEvent, matched, read
}

// matchBody is matchOnce for the cases that assert on the block alone, with nothing held back.
func matchBody(format logFormat, m eventMatch, data string) (string, int) {
	events, _, _, matched := matchEvents([]byte(data), format, m, &tailRead{})

	return string(bytes.Join(events, nil)), matched
}

func TestDeadlocksArtifact(t *testing.T) {
	artifact := NewDeadlocks().Artifact()

	assert.Equal(t, "pg_deadlocks", artifact.Name)
	assert.Equal(t, "pg_deadlocks.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope)
	assert.Equal(t, formatText, artifact.Format,
		"the body is opaque bytes, and a receiver dispatching on the first block's format= "+
			"would otherwise parse this file as CSV")

	assert.Equal(t, Every(DefaultLogTailInterval), artifact.Schedule)
	assert.Equal(t, LogDrainBudget, artifact.SampleBudget)

	var _ Collector = NewDeadlocks()
	var _ Closing = NewDeadlocks()
}

func TestDeadlocksTakesTheWholeReportAndStopsAtTheRightLine(t *testing.T) {
	body, pending, _, matched, _ := matchOnce(logFormatStderr, deadlockMatch, measuredDeadlock+unrelatedTraffic)

	require.Equal(t, 1, matched)
	assert.Equal(t, measuredDeadlock, body,
		"the report continues past DETAIL into HINT, CONTEXT and STATEMENT - CONTEXT names the "+
			"relation and the tuple, STATEMENT carries the victim's own statement, and a "+
			"DETAIL-only rule discards both")
	assert.Equal(t, len(measuredDeadlock), len(body))
	assert.Empty(t, pending)

	assert.Contains(t, body, `while updating tuple (0,1) in relation "yc_dl"`)
	assert.True(t, strings.HasSuffix(body, "COMMIT;\n"), "and it ends at the STATEMENT: line")
}

func TestDeadlocksSeparatorToleranceSurvivesTheMeasuredGap(t *testing.T) {
	oneSpace := strings.ReplaceAll(measuredDeadlock, "ERROR:  ", "ERROR: ")
	require.NotEqual(t, measuredDeadlock, oneSpace)

	for name, fixture := range map[string]string{
		"measured two spaces": measuredDeadlock,
		"one space":           oneSpace,
	} {
		t.Run(name, func(t *testing.T) {
			_, matched := matchBody(logFormatStderr, deadlockMatch, fixture+unrelatedTraffic)
			assert.Equal(t, 1, matched)
		})
	}
}

func TestDeadlocksTranslatedReportMatchesNothingAndBreaksNothing(t *testing.T) {
	translated := "2026-08-15 10:00:34.543 UTC [25666] FEHLER:  Verklemmung (Deadlock) entdeckt\n" +
		"2026-08-15 10:00:34.543 UTC [25666] DETAIL:  Prozess 25666 wartet auf ShareLock.\n" +
		"\tProzess 25651 wartet auf ShareLock.\n" +
		"2026-08-15 10:00:34.543 UTC [25666] ANWEISUNG:  UPDATE yc_dl SET v=2 WHERE id=2;\n"

	body, pending, pendingIsEvent, matched, _ := matchOnce(logFormatStderr, deadlockMatch, translated)

	assert.Zero(t, matched, "a known blind spot, pinned as exactly a blind spot")
	assert.Empty(t, body)
	assert.Empty(t, pending)
	assert.False(t, pendingIsEvent, "and nothing half-bounded is held back")
}

func TestDeadlocksTabRuleDoesNotDependOnTheLogLinePrefix(t *testing.T) {
	unprefixed := "ERROR:  deadlock detected\n" +
		"DETAIL:  Process 25666 waits for ShareLock on transaction 948; blocked by process 25651.\n" +
		"\tProcess 25651 waits for ShareLock on transaction 949; blocked by process 25666.\n" +
		"\tProcess 25666: BEGIN; UPDATE yc_dl SET v=2 WHERE id=2;\n" +
		"\tProcess 25651: BEGIN; UPDATE yc_dl SET v=1 WHERE id=1;\n" +
		"STATEMENT:  BEGIN; UPDATE yc_dl SET v=2 WHERE id=2;\n"

	body, matched := matchBody(logFormatStderr, deadlockMatch, unprefixed+"LOG:  checkpoint starting: time\n")

	require.Equal(t, 1, matched)
	assert.Equal(t, unprefixed, body)
	assert.Equal(t, 4, strings.Count(body, "\n\tProcess")+1, "all four DETAIL lines survive")
}

func TestDeadlocksMultiLineStatementIsOneEvent(t *testing.T) {
	multiline := "2026-08-15 10:00:34.543 UTC [25666] ERROR:  deadlock detected\n" +
		"2026-08-15 10:00:34.543 UTC [25666] STATEMENT:  SELECT 1,\n" +
		"\t2,\n" +
		"\tbadcol\n" +
		"\tFROM yc_dl;\n"

	body, matched := matchBody(logFormatStderr, deadlockMatch, multiline+unrelatedTraffic)

	assert.Equal(t, 1, matched, "one event, not three")
	assert.Equal(t, multiline, body)
}

func TestDeadlocksBoundaryBiasIsToOverCopy(t *testing.T) {
	withStray := "2026-08-15 10:00:34.543 UTC [25666] ERROR:  deadlock detected\n" +
		"a line from nowhere that names no severity\n" +
		"2026-08-15 10:00:35.000 UTC [25666] LOG:  checkpoint starting: time\n"

	body, matched := matchBody(logFormatStderr, deadlockMatch, withStray)

	require.Equal(t, 1, matched)
	assert.Contains(t, body, "a line from nowhere")
	assert.NotContains(t, body, "checkpoint starting", "and the next real entry still ends it")
}

func TestDeadlocksSecondaryTextContainingASeverityDoesNotEndTheEvent(t *testing.T) {
	tricky := "2026-08-15 10:00:34.543 UTC [25666] ERROR:  deadlock detected\n" +
		"2026-08-15 10:00:34.543 UTC [25666] STATEMENT:  INSERT INTO audit(msg) VALUES ('ERROR:  boom');\n"

	body, matched := matchBody(logFormatStderr, deadlockMatch, tricky+unrelatedTraffic)

	require.Equal(t, 1, matched)
	assert.Equal(t, tricky, body,
		"the keyword is tested at the report's own prefix, not scanned for anywhere in the line")
}

func TestDeadlocksInEveryFormat(t *testing.T) {
	twentySeven := strings.TrimSuffix(measuredDeadlockCSV, "\n") + ",\"extra column a future version appended\"\n"

	for _, tt := range []struct {
		name      string
		format    logFormat
		fixture   string
		unrelated string
		matchedBy string
	}{
		{name: "stderr", format: logFormatStderr, fixture: measuredDeadlock, unrelated: unrelatedTraffic, matchedBy: matchedByMessage},
		{name: "csvlog", format: logFormatCSV, fixture: measuredDeadlockCSV, unrelated: unrelatedCSV, matchedBy: matchedBySQLState},
		{name: "csvlog with a column appended", format: logFormatCSV, fixture: twentySeven, unrelated: unrelatedCSV, matchedBy: matchedBySQLState},
		{name: "jsonlog", format: logFormatJSON, fixture: measuredDeadlockJSON, unrelated: unrelatedJSON, matchedBy: matchedBySQLState},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, matched := matchBody(tt.format, deadlockMatch, tt.fixture+tt.unrelated)

			require.Equal(t, 1, matched)
			assert.Equal(t, tt.fixture, body,
				"the record's original bytes, never a re-encoding of what the parser understood")

			tail := newLogTail("pg_deadlocks", deadlockMatch)
			tail.source.format = tt.format

			assert.Equal(t, tt.matchedBy, tail.matchedBy(),
				"a code stands alone for this event, so the format decides what the header says")
		})
	}
}

func TestDeadlocksCSVRecordSplitMidQuoteParsesWhereItCompleted(t *testing.T) {
	cut := strings.Index(measuredDeadlockCSV, "Process 105 waits")
	require.Positive(t, cut)

	first, second := measuredDeadlockCSV[:cut], measuredDeadlockCSV[cut:]

	body, pending, pendingIsEvent, matched, _ := matchOnce(logFormatCSV, deadlockMatch, first)

	require.Zero(t, matched, "an incomplete record is held, matched or not")
	assert.Empty(t, body)
	assert.Equal(t, first, pending)
	assert.False(t, pendingIsEvent)

	body, matched = matchBody(logFormatCSV, deadlockMatch, pending+second+unrelatedCSV)

	assert.Equal(t, 1, matched, "and parses whole in the read where it completed")
	assert.Equal(t, measuredDeadlockCSV, body)
}

func TestDeadlocksTwoEventsInOneRead(t *testing.T) {
	body, matched := matchBody(logFormatStderr, deadlockMatch,
		measuredDeadlock+unrelatedTraffic+measuredDeadlock+unrelatedTraffic)

	assert.Equal(t, 2, matched)
	assert.Equal(t, measuredDeadlock+measuredDeadlock, body, "two contiguous bodies in one block")
}

func TestDeadlocksMaxEventBytesTruncatesInBytes(t *testing.T) {
	invalid := string([]byte{0xff, 0xfe, 0xfd})

	huge := "2026-08-15 10:00:34.543 UTC [25666] ERROR:  deadlock detected\n" +
		"2026-08-15 10:00:34.543 UTC [25666] STATEMENT:  INSERT INTO t VALUES ('" +
		invalid + strings.Repeat("x", MaxEventBytes) + "');\n"

	body, _, _, matched, read := matchOnce(logFormatStderr, deadlockMatch, huge+unrelatedTraffic)

	require.Equal(t, 1, matched)
	assert.Equal(t, 1, read.eventsTruncated)

	assert.Equal(t, MaxEventBytes+1, len(body), "the cap, in bytes, plus the newline the body always ends with")
	assert.True(t, strings.HasSuffix(body, eventTruncationMark+"\n"), "with the mark counted inside it")
	assert.Contains(t, body, invalid, "and the bytes that were kept are un-rewritten")
}

func TestDeadlocksMaxEventLinesBoundsAnEventWithNoEnd(t *testing.T) {
	endless := "2026-08-15 10:00:34.543 UTC [25666] ERROR:  deadlock detected\n" +
		strings.Repeat("\tstill the same report\n", MaxEventLines*2)

	body, matched := matchBody(logFormatStderr, deadlockMatch, endless)

	require.Equal(t, 1, matched)
	assert.Equal(t, MaxEventLines, strings.Count(body, "\n"),
		"a matcher that cannot prove a boundary must not copy the rest of the file into one block")
}

func TestDeadlocksBytesEqualsTheBodyLengthOnEveryBlock(t *testing.T) {
	hashPrefixed := strings.ReplaceAll(measuredDeadlock, "2026-08-15 10:00:34.543 UTC [25666] ",
		"#2026-08-15 10:00:34.543 UTC [25666] ")

	invalid := "2026-08-15 10:00:34.543 UTC [25666] ERROR:  deadlock detected\n" +
		"2026-08-15 10:00:34.543 UTC [25666] STATEMENT:  SELECT '" + string([]byte{0xff, 0xfe}) + "';\n"

	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	h := newDeadlockHarness(t, deniedQuerier(dir.settings()))

	blocks := []textBlock{h.next()}

	for _, fixture := range []string{hashPrefixed, invalid, ""} {
		dir.append(fixture + unrelatedTraffic)
		blocks = append(blocks, h.next())
	}

	require.Equal(t, hashPrefixed, blocks[1].body,
		"a body line beginning with '#' is why bytes= and not a scanning parser")
	require.Equal(t, invalid, blocks[2].body)
	require.Empty(t, blocks[3].body)

	for _, block := range blocks {
		size, err := strconv.Atoi(block.fields["bytes"])
		require.NoError(t, err)
		assert.Equal(t, size, len(block.body), block.header)
	}
}

func TestDeadlocksArtifactCapStopsMatchingAndSaysSo(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	collector := NewDeadlocks()
	h := &tailHarness{t: t, collector: collector, q: deniedQuerier(dir.settings())}

	h.next()

	collector.tail.written = MaxArtifactBytes
	collector.tail.full = true

	dir.append(measuredDeadlock + unrelatedTraffic)

	block := h.next()

	assert.Equal(t, "true", block.fields["artifact_full"])
	assert.Equal(t, "0", block.fields["matched"])
	assert.NotEqual(t, block.fields["from_offset"], block.fields["to_offset"],
		"offsets still advance, so the file stays an honest record of what was passed over")
}

var deadlocksGoldenWrites = []string{
	unrelatedTraffic,
	measuredDeadlock + unrelatedTraffic,
	unrelatedTraffic,
}

func TestDeadlocksGoldenFull(t *testing.T) {
	results := runLogGoldenWindow(t, NewDeadlocks(), logFormatStderr,
		priorTraffic, deadlocksGoldenWrites, 30*time.Second, logGoldenClock(t, 3))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 3, results[0].SamplesWritten, "the drain is not a sample")

	assert.Equal(t, bloatGolden(t, "pg_deadlocks_full.txt"), artifactText(t, results[0]))
}

func TestDeadlocksGoldenCSVLog(t *testing.T) {
	results := runLogGoldenWindow(t, NewDeadlocks(), logFormatCSV,
		"", []string{measuredDeadlockCSV + unrelatedCSV}, 20*time.Second, logGoldenClock(t, 2))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_deadlocks_csvlog.txt"), artifactText(t, results[0]))
}

func TestDeadlocksGoldenJSONLog(t *testing.T) {
	results := runLogGoldenWindow(t, NewDeadlocks(), logFormatJSON,
		"", []string{measuredDeadlockJSON + unrelatedJSON}, 20*time.Second, logGoldenClock(t, 2))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_deadlocks_jsonlog.txt"), artifactText(t, results[0]))
}

func TestDeadlocksGoldenRemote(t *testing.T) {
	results := runRemoteGoldenWindow(t, NewDeadlocks(), 120*time.Second, logGoldenClock(t, 12))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 12, results[0].SamplesWritten,
		"status=complete is honest: every scheduled sample was written, and the artifact's "+
			"content is the reason it is empty")

	artifact := artifactText(t, results[0])

	assert.NotContains(t, artifact, "matched=",
		"there is no matched= key anywhere in this file, and that is the design: a receiver "+
			"cannot render a zero it was never given")

	assert.Equal(t, bloatGolden(t, "pg_deadlocks_remote.txt"), artifact)
}

func formatExtension(format logFormat) string {
	switch format {
	case logFormatCSV:
		return ".csv"
	case logFormatJSON:
		return ".json"
	}

	return ".log"
}
