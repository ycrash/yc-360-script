//go:build windows

package capture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yc-agent/internal/capture/executils"
)

// Differential tests: run the real wmic beside the replacement and compare.
// They skip where wmic is gone (Windows 11 24H2/25H2), so they earn their keep
// on machines that still have it; the recorded fixtures cover the rest.

// wmicQueries are the queries the fixtures are recorded from, covering many
// rows, several columns, blank cells, values shorter than their header, and a
// query matching nothing. None returns a path or serial, so they are safe to
// commit.
var wmicQueries = []struct {
	name string
	args []string
}{
	{name: "logicaldisk_three_columns", args: []string{"logicaldisk", "get", "size,freespace,caption"}},
	{name: "logicaldisk_four_columns", args: []string{"logicaldisk", "get", "Caption,VolumeName,FileSystem,Compressed"}},
	{name: "process_many_rows", args: []string{"process", "get", "ProcessId,Name,Priority"}},
	{name: "os_blank_cells", args: []string{"os", "get", "Caption,CSDVersion,OtherTypeDescription"}},
	{name: "os_short_values", args: []string{"os", "get", "Primary,Distributed"}},
	{name: "cpu_five_columns", args: []string{"cpu", "get", "DeviceID,Name,NumberOfCores,Status,StatusInfo"}},
	{name: "volume_blank_labels", args: []string{"volume", "get", "DriveLetter,Label,FileSystem,Capacity"}},
	{name: "nicconfig_several_rows", args: []string{"nicconfig", "get", "Index,IPEnabled,DHCPEnabled"}},
	{name: "zero_instances", args: []string{"logicaldisk", "where", "Caption='ZZ:'", "get", "size,freespace,caption"}},
}

func requireWMIC(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("wmic"); err != nil {
		t.Skip("wmic is not installed here; the recorded fixtures in TestWMICCharacterization cover this instead")
	}
}

// TestWMICLayoutRoundTripLive is TestWMICCharacterization against live wmic:
// whatever tables this machine produces must rebuild byte for byte.
func TestWMICLayoutRoundTripLive(t *testing.T) {
	requireWMIC(t)

	for _, q := range wmicQueries {
		t.Run(q.name, func(t *testing.T) {
			out, err := exec.Command("wmic", q.args...).Output()
			require.NoError(t, err)

			table, err := parseWMICTable(string(out))
			require.NoError(t, err)

			rebuilt := formatWMICTable(table.Headers, table.Rows)

			assert.Equal(t, string(out), string(rebuilt),
				"formatWMICTable does not reproduce live wmic output for %v", q.args)
		})
	}
}

// TestLogicalDiskSetMatchesWMIC checks the drives listed, and their order,
// match wmic's - catching a drive silently dropped, as gopsutil would.
func TestLogicalDiskSetMatchesWMIC(t *testing.T) {
	requireWMIC(t)

	table := liveLogicalDiskTable(t)

	var want []string
	for _, row := range table.Rows {
		want = append(want, row[0])
	}

	disks, err := logicalDisks()
	require.NoError(t, err)

	var got []string
	for _, disk := range disks {
		got = append(got, disk.Caption)
	}

	assert.Equal(t, want, got, "the drives we enumerate differ from the ones wmic reported")
}

// TestLogicalDiskValuesMatchWMIC checks values, not layout. Size must match
// exactly; FreeSpace moves, so being close still catches the wrong field.
func TestLogicalDiskValuesMatchWMIC(t *testing.T) {
	requireWMIC(t)

	table := liveLogicalDiskTable(t)

	disks, err := logicalDisks()
	require.NoError(t, err)
	require.Equal(t, len(table.Rows), len(disks))

	for i, row := range table.Rows {
		caption, wantFree, wantSize := row[0], row[1], row[2]
		got := disks[i]

		require.Equal(t, caption, got.Caption)
		assert.Equal(t, wantSize, got.Size, "Size differs for %s", caption)

		// A drive wmic could not measure must come back blank, not zero.
		if wantSize == "" {
			assert.Empty(t, got.Size, "%s is unmeasurable for wmic but not for us", caption)
			assert.Empty(t, got.FreeSpace, "%s is unmeasurable for wmic but not for us", caption)
			continue
		}

		wantN, err := strconv.ParseUint(wantFree, 10, 64)
		require.NoError(t, err)
		gotN, err := strconv.ParseUint(got.FreeSpace, 10, 64)
		require.NoError(t, err)

		// A gibibyte is generous for the milliseconds between the two calls,
		// and far tighter than reading the wrong field would be.
		assert.InDelta(t, wantN, gotN, 1<<30, "FreeSpace differs too much for %s", caption)
	}
}

// TestDiskCaptureMatchesWMIC is the end-to-end check: the file the agent
// writes against wmic's bytes, with free space held constant.
func TestDiskCaptureMatchesWMIC(t *testing.T) {
	requireWMIC(t)

	want, err := executils.CommandCombinedOutput(executils.Command{
		"wmic", "logicaldisk", "get", "size,freespace,caption",
	})
	require.NoError(t, err)

	dir := t.TempDir()
	cwd, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	defer os.Chdir(cwd)

	file, err := (&Disk{}).CaptureToFile()
	require.NoError(t, err)
	defer file.Close()

	got, err := os.ReadFile(file.Name())
	require.NoError(t, err)

	assert.Equal(t, len(want), len(got), "capture is a different length to wmic's output")
	assert.Equal(t, holdFreeSpaceConstant(t, string(want)), holdFreeSpaceConstant(t, string(got)))
}

// liveLogicalDiskTable runs the query the agent used to run and takes the
// result apart.
func liveLogicalDiskTable(t *testing.T) wmicTable {
	t.Helper()

	out, err := exec.Command("wmic", "logicaldisk", "get", "size,freespace,caption").Output()
	require.NoError(t, err)

	table, err := parseWMICTable(string(out))
	require.NoError(t, err)
	require.NotEmpty(t, table.Rows, "this machine reports no logical disks")

	return table
}

// holdFreeSpaceConstant zeroes the FreeSpace column so two captures taken
// moments apart compare equal, rebuilding through formatWMICTable so a
// padding difference still shows up.
func holdFreeSpaceConstant(t *testing.T, out string) string {
	t.Helper()

	table, err := parseWMICTable(out)
	require.NoError(t, err)

	for _, row := range table.Rows {
		if len(row) > 1 && row[1] != "" {
			row[1] = strings.Repeat("0", len(row[1]))
		}
	}

	return string(formatWMICTable(table.Headers, table.Rows))
}

// TestRecordWMICFixtures regenerates testdata/wmic. Skipped unless asked, since
// it rewrites committed files:
//
//	YC_RECORD_WMIC_FIXTURES=1 go test ./internal/capture/ -run TestRecordWMICFixtures
//
// Run only where wmic still exists, and review the diff.
//
// It skips logicaldisk_unmeasurable_drive.txt, which needs a drive the OS
// cannot measure. To make one without admin rights or special hardware, point
// a subst drive at a directory and delete the directory - it lingers as
// DriveType 1, which wmic lists with Size and FreeSpace blank:
//
//	mkdir %TEMP%\dangling
//	subst Y: %TEMP%\dangling
//	rmdir /s /q %TEMP%\dangling
//	wmic logicaldisk get size,freespace,caption > testdata\wmic\logicaldisk_unmeasurable_drive.txt
//	subst Y: /D
//
// Redirect from cmd, not PowerShell, which would re-encode the bytes.
func TestRecordWMICFixtures(t *testing.T) {
	if os.Getenv("YC_RECORD_WMIC_FIXTURES") == "" {
		t.Skip("set YC_RECORD_WMIC_FIXTURES=1 to re-record the wmic fixtures")
	}
	requireWMIC(t)

	dir := wmicFixtureDir()
	require.NoError(t, os.MkdirAll(dir, 0o755))

	for _, q := range wmicQueries {
		t.Run(q.name, func(t *testing.T) {
			// Output, not CombinedOutput: a query matching nothing writes
			// "No Instance(s) Available." to stderr, not part of the table.
			out, err := exec.Command("wmic", q.args...).Output()
			require.NoError(t, err)
			require.NotEmpty(t, out)

			path := filepath.Join(dir, q.name+".txt")
			require.NoError(t, os.WriteFile(path, out, 0o644))
			t.Logf("recorded %d bytes from wmic %s", len(out), strings.Join(q.args, " "))
		})
	}
}
