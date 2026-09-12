package capture

// Backward compatibility with wmic's output format. WMIC is gone from Windows
// 11 24H2/25H2, but the server-side "df" parser still expects the exact bytes
// `wmic logicaldisk get` produced, so disk.out keeps reproducing them.

import (
	"bytes"
)

// logicalDisk is one row of the logical disk table written to disk.out.
//
// FreeSpace and Size are decimal strings, not numbers: wmic printed a blank
// cell for a drive whose size it could not read (a slot holding no media) and
// listed the drive anyway. An empty string here is that blank cell.
type logicalDisk struct {
	Caption   string
	FreeSpace string
	Size      string
}

// logicalDiskHeaders are the column headers in wmic's order: it printed the
// requested properties alphabetically whatever order they were asked in, so
// "get size,freespace,caption" produced Caption, FreeSpace, Size.
var logicalDiskHeaders = []string{"Caption", "FreeSpace", "Size"}

// wmicLineEnding ends every line, the trailing one included. The doubled CR is
// not a typo: wmic wrote "\r\n" through a CRT text-mode stream, which
// translated the "\n" again. The server-side "df" parser expects it.
const wmicLineEnding = "\r\r\n"

// formatLogicalDiskTable renders the table that
// `wmic logicaldisk get size,freespace,caption` used to produce.
func formatLogicalDiskTable(disks []logicalDisk) []byte {
	rows := make([][]string, 0, len(disks))
	for _, disk := range disks {
		rows = append(rows, []string{disk.Caption, disk.FreeSpace, disk.Size})
	}

	return formatWMICTable(logicalDiskHeaders, rows)
}

// formatWMICTable renders headers and rows exactly as a wmic `get` query did
// when read through a pipe, which is how the agent always invoked it:
//
//   - plain ASCII, no BOM (wmic used UTF-16LE+BOM only for a file handle)
//   - every cell left-aligned and space-padded, the last column included, to
//     max(len(header), widest value) + 2
//   - wmicLineEnding after every row, plus one more as a trailing blank line
//   - with no rows, no header line at all - just the two line endings
//
// The characterization tests verify this byte for byte against recorded wmic
// output.
func formatWMICTable(headers []string, rows [][]string) []byte {
	if len(rows) == 0 {
		return []byte(wmicLineEnding + wmicLineEnding)
	}

	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = len(header)
		for _, row := range rows {
			if i < len(row) && len(row[i]) > widths[i] {
				widths[i] = len(row[i])
			}
		}
		widths[i] += 2
	}

	var buf bytes.Buffer
	writeRow := func(cells []string) {
		for i := range headers {
			var cell string
			if i < len(cells) {
				cell = cells[i]
			}
			buf.WriteString(cell)
			for pad := widths[i] - len(cell); pad > 0; pad-- {
				buf.WriteByte(' ')
			}
		}
		buf.WriteString(wmicLineEnding)
	}

	writeRow(headers)
	for _, row := range rows {
		writeRow(row)
	}
	buf.WriteString(wmicLineEnding)

	return buf.Bytes()
}
