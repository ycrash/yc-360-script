package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Characterization tests: testdata/wmic holds tables recorded from a real wmic
// (TestRecordWMICFixtures re-records). Each is taken apart and rebuilt with
// formatWMICTable, which must reproduce it byte for byte, with no wmic needed.

// packageDir is captured before any test runs: several tests here os.Chdir
// into a temp directory, so fixture paths cannot use the current directory.
var packageDir = func() string {
	dir, err := os.Getwd()
	if err != nil {
		panic("cannot determine the test package directory: " + err.Error())
	}
	return dir
}()

// wmicFixtureDir is where the recorded wmic output lives.
func wmicFixtureDir() string {
	return filepath.Join(packageDir, "testdata", "wmic")
}

// wmicTable is a wmic `get` table taken apart into the pieces formatWMICTable
// needs in order to rebuild it.
type wmicTable struct {
	Headers []string
	Rows    [][]string
}

// parseWMICTable takes wmic `get` output apart into headers and rows. Column
// boundaries come from the header line, since every cell starts at its own
// header's offset - splitting on whitespace would break a value with spaces.
func parseWMICTable(out string) (wmicTable, error) {
	if !strings.HasSuffix(out, wmicLineEnding+wmicLineEnding) {
		return wmicTable{}, fmt.Errorf("output does not end with a trailing blank line")
	}

	// Drop the trailing blank line, then the final row's own line ending.
	body := strings.TrimSuffix(out, wmicLineEnding)
	body = strings.TrimSuffix(body, wmicLineEnding)
	if body == "" {
		// wmic printed no header line at all when a query matched nothing.
		return wmicTable{}, nil
	}

	lines := strings.Split(body, wmicLineEnding)
	starts := columnStarts(lines[0])
	if len(starts) == 0 {
		return wmicTable{}, fmt.Errorf("header line has no columns: %q", lines[0])
	}

	cells := func(line string) []string {
		out := make([]string, len(starts))
		for i, start := range starts {
			end := len(line)
			if i+1 < len(starts) && starts[i+1] < end {
				end = starts[i+1]
			}
			if start > len(line) {
				continue
			}
			out[i] = strings.TrimRight(line[start:end], " ")
		}
		return out
	}

	table := wmicTable{Headers: cells(lines[0])}
	for _, line := range lines[1:] {
		table.Rows = append(table.Rows, cells(line))
	}

	return table, nil
}

// columnStarts returns each column's start offset. wmic property names never
// contain a space, so each run of non-space in the header is one column.
func columnStarts(header string) []int {
	var starts []int

	inWord := false
	for i := 0; i < len(header); i++ {
		if header[i] == ' ' {
			inWord = false
			continue
		}
		if !inWord {
			starts = append(starts, i)
			inWord = true
		}
	}

	return starts
}

// TestWMICCharacterization rebuilds every recorded table and requires the
// bytes to be identical to what wmic produced.
func TestWMICCharacterization(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join(wmicFixtureDir(), "*.txt"))
	require.NoError(t, err)
	require.NotEmpty(t, fixtures, "no wmic fixtures recorded under testdata/wmic")

	for _, fixture := range fixtures {
		t.Run(strings.TrimSuffix(filepath.Base(fixture), ".txt"), func(t *testing.T) {
			raw, err := os.ReadFile(fixture)
			require.NoError(t, err)
			recorded := string(raw)

			// The "\r\r\n" endings survive only because .gitattributes marks
			// this directory binary. Fail loudly if that is ever lost.
			require.NotContains(t, recorded, "\r\r\r\n",
				"fixture line endings were mangled - check .gitattributes marks testdata/wmic as -text")
			require.True(t, strings.HasSuffix(recorded, wmicLineEnding+wmicLineEnding),
				"fixture does not end the way wmic ended its output")

			table, err := parseWMICTable(recorded)
			require.NoError(t, err)

			rebuilt := formatWMICTable(table.Headers, table.Rows)

			assert.Equal(t, recorded, string(rebuilt),
				"formatWMICTable no longer reproduces what wmic produced for this query")
		})
	}
}

// hasUnmeasurableDriveFixture reports whether a recorded logical disk table
// has a drive with a blank Size - the empty optical or card reader slot that
// gopsutil dropped and logicalDisks keeps.
func hasUnmeasurableDriveFixture(t *testing.T) bool {
	t.Helper()

	fixtures, err := filepath.Glob(filepath.Join(wmicFixtureDir(), "logicaldisk_*.txt"))
	require.NoError(t, err)

	for _, fixture := range fixtures {
		raw, err := os.ReadFile(fixture)
		require.NoError(t, err)

		table, err := parseWMICTable(string(raw))
		require.NoError(t, err, fixture)

		caption, size := indexOfHeader(table.Headers, "Caption"), indexOfHeader(table.Headers, "Size")
		if caption < 0 || size < 0 {
			continue
		}

		for _, row := range table.Rows {
			if row[caption] != "" && row[size] == "" {
				return true
			}
		}
	}

	return false
}

// indexOfHeader returns the column index of name, or -1.
func indexOfHeader(headers []string, name string) int {
	for i, header := range headers {
		if header == name {
			return i
		}
	}

	return -1
}

// TestWMICCharacterizationCoversItsEdges guards the fixture set: a re-record
// that lost the interesting shapes would still pass the suite above.
func TestWMICCharacterizationCoversItsEdges(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join(wmicFixtureDir(), "*.txt"))
	require.NoError(t, err)

	var (
		maxRows, maxCols int
		sawBlankCell     bool
		sawValueWider    bool
		sawValueShorter  bool
		sawZeroRows      bool
	)

	for _, fixture := range fixtures {
		raw, err := os.ReadFile(fixture)
		require.NoError(t, err)

		table, err := parseWMICTable(string(raw))
		require.NoError(t, err, fixture)

		if len(table.Rows) == 0 {
			sawZeroRows = true
			continue
		}
		if len(table.Rows) > maxRows {
			maxRows = len(table.Rows)
		}
		if len(table.Headers) > maxCols {
			maxCols = len(table.Headers)
		}

		for i, header := range table.Headers {
			widest := 0
			for _, row := range table.Rows {
				if i >= len(row) {
					continue
				}
				if row[i] == "" {
					sawBlankCell = true
				}
				if len(row[i]) > widest {
					widest = len(row[i])
				}
			}
			if widest > len(header) {
				sawValueWider = true
			}
			if widest > 0 && widest < len(header) {
				sawValueShorter = true
			}
		}
	}

	assert.GreaterOrEqual(t, maxRows, 100, "no large multi-row table recorded")
	assert.True(t, hasUnmeasurableDriveFixture(t),
		"no blank-size drive recorded, and it cannot be re-recorded once wmic is gone; "+
			"see TestRecordWMICFixtures to reproduce the drive")
	assert.GreaterOrEqual(t, maxCols, 4, "no wide table recorded")
	assert.True(t, sawBlankCell, "no table with a blank cell recorded (the empty-drive shape)")
	assert.True(t, sawValueWider, "no column whose widest value exceeds its header")
	assert.True(t, sawValueShorter, "no column whose values are shorter than its header")
	assert.True(t, sawZeroRows, "no zero-row table recorded")
}
