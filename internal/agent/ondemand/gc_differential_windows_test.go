//go:build windows

package ondemand

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	ps "github.com/shirou/gopsutil/v3/process"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yc-agent/internal/capture/executils"
)

// GetGCLogFile uses a command line for one thing: finding the -Xloggc /
// -Xlog:gc path. So the replacement must yield the same answer from
// ExtractGCLogPathFromCmdline, not the same bytes - wmic wrapped the value in
// a padded table. These tests check that across every process on the machine,
// and skip where wmic is gone.

func requireWMIC(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("wmic"); err != nil {
		t.Skip("wmic is not installed here, so there is nothing to compare against")
	}
}

// cmdlineFromWMIC reads a process command line the way the agent used to,
// returning "" when wmic reported no such process.
func cmdlineFromWMIC(t *testing.T, pid int32) string {
	t.Helper()

	out, err := exec.Command("wmic", "process", "where",
		"ProcessId="+strconv.Itoa(int(pid)), "get", "ProcessId,Commandline").Output()
	if err != nil {
		return ""
	}

	return string(out)
}

// TestGCLogPathAgreesAcrossSources is the characterization test proper: for
// every process this machine reports, the GC log path from wmic's output must
// equal the one from gopsutil's. A process a source will not report is skipped
// rather than failed - access legitimately differs between them, which is why
// the implementation keeps two sources.
func TestGCLogPathAgreesAcrossSources(t *testing.T) {
	requireWMIC(t)

	procs, err := ps.Processes()
	require.NoError(t, err)
	require.NotEmpty(t, procs)

	var compared, skipped int

	for _, p := range procs {
		wmicOut := cmdlineFromWMIC(t, p.Pid)
		if strings.TrimSpace(wmicOut) == "" {
			skipped++
			continue
		}

		gopsutilOut, err := cmdlineFromGopsutil(int(p.Pid))
		if err != nil {
			skipped++
			continue
		}

		want := ExtractGCLogPathFromCmdline(wmicOut)
		got := ExtractGCLogPathFromCmdline(string(gopsutilOut))

		assert.Equal(t, want, got,
			"pid %d: gopsutil yields a different GC log path to wmic", p.Pid)
		compared++
	}

	t.Logf("compared %d processes against wmic, skipped %d that a source would not report", compared, skipped)
	assert.Greater(t, compared, 10, "too few processes were comparable for this to mean anything")
}

// TestCIMBackstopAgreesWithWMIC covers the other source, the CIM query in
// executils.GC behind gopsutil. Fewer processes: each costs a PowerShell start.
func TestCIMBackstopAgreesWithWMIC(t *testing.T) {
	requireWMIC(t)

	procs, err := ps.Processes()
	require.NoError(t, err)

	var compared int
	for _, p := range procs {
		if compared >= 5 {
			break
		}

		wmicOut := cmdlineFromWMIC(t, p.Pid)
		if strings.TrimSpace(wmicOut) == "" {
			continue
		}

		cimOut, err := cmdlineFromCommand(int(p.Pid))
		if err != nil {
			continue
		}

		assert.Equal(t,
			ExtractGCLogPathFromCmdline(wmicOut),
			ExtractGCLogPathFromCmdline(string(cimOut)),
			"pid %d: the CIM backstop yields a different GC log path to wmic", p.Pid)
		compared++
	}

	assert.Greater(t, compared, 0, "no process could be compared through the CIM backstop")
}

// TestReplacementSourcesDoNotTruncate guards the way a PowerShell replacement
// can silently corrupt this: default formatting wraps a long value at the
// console width and splits it mid-token, cutting a long -Xloggc path in half.
// A command line long enough to trigger that must come back whole.
func TestReplacementSourcesDoNotTruncate(t *testing.T) {
	procs, err := ps.Processes()
	require.NoError(t, err)

	// Both sides trimmed: a command line legitimately ends in a space often
	// enough (six processes on the machine this was written on), and the CIM
	// output carries a trailing CRLF, so comparing trimmed against untrimmed
	// would fail on a value that was never truncated.
	var longest int
	var longestPid int32
	for _, p := range procs {
		out, err := cmdlineFromGopsutil(int(p.Pid))
		if err != nil {
			continue
		}
		if n := len(strings.TrimSpace(string(out))); n > longest {
			longest, longestPid = n, p.Pid
		}
	}

	require.Greater(t, longest, 200,
		"no process on this machine has a command line long enough to test wrapping")

	cimOut, err := cmdlineFromCommand(int(longestPid))
	require.NoError(t, err)

	cim := strings.TrimSpace(string(cimOut))

	// If the process exited between being measured and queried the CIM query
	// reports nothing, so there is no truncation to judge either way.
	if cim == "" {
		t.Skipf("pid %d exited before the CIM query could read it", longestPid)
	}

	// Wrapping inserts a newline plus padding spaces mid-value. A command line
	// is a single line; anything else means the formatting mangled it.
	assert.NotContains(t, cim, "\r\n ", "the CIM query's output was wrapped and padded")
	assert.Equal(t, len(cim), len(strings.ReplaceAll(cim, "\n", "")),
		"the CIM query's output spans multiple lines, so it was wrapped")
	assert.GreaterOrEqual(t, len(cim), longest,
		"the CIM query returned a shorter command line than gopsutil, so it was truncated")
}

// gcFlagCases are the JVM logging flags GetGCLogFile has to recognise, with
// the path each one should yield.
var gcFlagCases = []struct {
	name string
	flag string
	want string
}{
	{name: "no gc flag", flag: "-Dsomething=else", want: ""},
	{name: "Xloggc", flag: `-Xloggc:C:\logs\garbage-collection.log`, want: `C:\logs\garbage-collection.log`},
	{name: "Xlog gc to file", flag: `-Xlog:gc:C:\logs\gctrace.log`, want: `C:\logs\gctrace.log`},
	{name: "Xlog gc trace with decorators", flag: `-Xlog:gc=trace:file=C:\logs\gctrace.txt:uptimemillis,pid:filecount=5`, want: `C:\logs\gctrace.txt`},
	{name: "Xverbosegclog with rotation", flag: `-Xverbosegclog:C:\logs\verbose.log,20,10`, want: `C:\logs\verbose.log`},
}

// TestGCLogPathFromLiveProcess reads the flag back off a running process
// through the whole GetGCLogFile path, end to end.
//
// It needs no JVM: the wmic removal changed how a command line is read, not
// how it is parsed, so any process carrying the flag exercises the code that
// changed. The helper is this test binary re-run in a mode that just sleeps.
func TestGCLogPathFromLiveProcess(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)

	for _, tc := range gcFlagCases {
		t.Run(tc.name, func(t *testing.T) {
			// After "--" so the test binary's flag parsing leaves it alone; it
			// is still part of the command line, which is all this needs.
			cmd := exec.Command(self, "-test.run=TestGCFlagCarrierHelper", "--", tc.flag)
			cmd.Env = append(os.Environ(), "YC_GC_FLAG_CARRIER=1")
			require.NoError(t, cmd.Start())

			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			})

			got, err := GetGCLogFile(cmd.Process.Pid)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestGCFlagCarrierHelper is not a test: it is the process
// TestGCLogPathFromLiveProcess starts, alive and carrying a JVM flag.
func TestGCFlagCarrierHelper(t *testing.T) {
	if os.Getenv("YC_GC_FLAG_CARRIER") == "" {
		t.Skip("helper process for TestGCLogPathFromLiveProcess")
	}

	time.Sleep(2 * time.Minute)
}

// TestGCCommandIsNotWMIC guards against an accidental revert: nothing in the
// Windows GC command may invoke wmic again.
func TestGCCommandIsNotWMIC(t *testing.T) {
	joined := strings.ToLower(strings.Join(executils.GC, " "))

	assert.NotContains(t, joined, "wmic",
		"executils.GC is back to using wmic, which Windows 11 24H2/25H2 no longer ships")
	assert.Contains(t, joined, "expandproperty",
		"the CIM query must expand the property; default formatting wraps long command lines")
}
