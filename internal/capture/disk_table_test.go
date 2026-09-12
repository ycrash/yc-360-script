package capture

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const crcrlf = "\r\r\n"

func TestFormatLogicalDiskTable(t *testing.T) {
	tests := []struct {
		name  string
		disks []logicalDisk
		want  string
	}{
		{
			// From a real `wmic logicaldisk get size,freespace,caption` on
			// Windows 11, read through a pipe as the agent read it. Widths are
			// max(header, widest value)+2: 9, 13, 14.
			name: "single drive matches wmic output",
			disks: []logicalDisk{
				{Caption: "C:", FreeSpace: "42720997376", Size: "136464822272"},
			},
			want: "Caption  FreeSpace    Size          " + crcrlf +
				"C:       42720997376  136464822272  " + crcrlf +
				crcrlf,
		},
		{
			// The widest value in a column sets that column's width.
			name: "column widths follow the widest value",
			disks: []logicalDisk{
				{Caption: "C:", FreeSpace: "42720997376", Size: "136464822272"},
				{Caption: "D:", FreeSpace: "512", Size: "1024"},
			},
			want: "Caption  FreeSpace    Size          " + crcrlf +
				"C:       42720997376  136464822272  " + crcrlf +
				"D:       512          1024          " + crcrlf +
				crcrlf,
		},
		{
			// The case gopsutil would have hidden: a slot with no media. wmic
			// kept the row, blank but still padded to full width.
			name: "unmeasurable drive keeps its row with blank cells",
			disks: []logicalDisk{
				{Caption: "C:", FreeSpace: "42720997376", Size: "136464822272"},
				{Caption: "D:"},
			},
			want: "Caption  FreeSpace    Size          " + crcrlf +
				"C:       42720997376  136464822272  " + crcrlf +
				"D:                                  " + crcrlf +
				crcrlf,
		},
		{
			// With every drive unmeasurable the headers alone set the widths.
			name: "all cells blank falls back to header widths",
			disks: []logicalDisk{
				{Caption: "D:"},
			},
			want: "Caption  FreeSpace  Size  " + crcrlf +
				"D:                        " + crcrlf +
				crcrlf,
		},
		{
			// wmic printed no header line for a query matching nothing.
			// Unreachable for logical disks, but reproduced for fidelity.
			name:  "no rows produces no header line",
			disks: nil,
			want:  crcrlf + crcrlf,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatLogicalDiskTable(tt.disks)

			assert.Equal(t, tt.want, string(got))
			assert.NotContains(t, string(got), "\x00", "piped wmic output was ASCII, never UTF-16")
		})
	}
}

// TestFormatWMICTableRowsAreEqualLength pins the property every wmic table
// had: every line is exactly as long as every other. A padding bug shows up
// here first.
func TestFormatWMICTableRowsAreEqualLength(t *testing.T) {
	out := formatWMICTable(
		[]string{"Caption", "FreeSpace", "Size"},
		[][]string{
			{"C:", "42720997376", "136464822272"},
			{"D:", "", ""},
			{"E:", "512", "1024"},
		},
	)

	// Drop the trailing blank line, then split into header plus rows.
	body := strings.TrimSuffix(string(out), crcrlf+crcrlf)
	lines := strings.Split(body, crcrlf)
	require.Len(t, lines, 4)

	for i, line := range lines[1:] {
		assert.Equal(t, len(lines[0]), len(line), "row %d is a different length to the header", i+1)
	}
}

// TestFormatWMICTableRoundTrips checks the renderer against the parser the
// characterization tests use, so a bug in one cannot cancel out the other.
func TestFormatWMICTableRoundTrips(t *testing.T) {
	headers := []string{"Caption", "FreeSpace", "Size"}
	rows := [][]string{
		{"C:", "42720997376", "136464822272"},
		{"D:", "", ""},
	}

	table, err := parseWMICTable(string(formatWMICTable(headers, rows)))
	require.NoError(t, err)

	assert.Equal(t, headers, table.Headers)
	assert.Equal(t, rows, table.Rows)
}
