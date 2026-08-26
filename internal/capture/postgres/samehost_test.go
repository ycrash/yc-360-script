package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInspector is a processInspector the test controls, so every branch runs
// without reading a real process table.
type fakeInspector struct {
	titles          bool
	byPID           map[int]string
	byNamespacedPID map[int]string
	seesForeign     bool
	container       bool
	parentStart     map[int]time.Time
}

func (f fakeInspector) titlesReadable() bool { return f.titles }

func (f fakeInspector) title(pid int) (string, bool) {
	title, ok := f.byPID[pid]

	return title, ok
}

func (f fakeInspector) canSeeForeignProcesses() bool { return f.seesForeign }

func (f fakeInspector) inContainer() bool { return f.container }

func (f fakeInspector) titleByNamespacedPID(pid int) (string, bool) {
	title, ok := f.byNamespacedPID[pid]

	return title, ok
}

func (f fakeInspector) parentStartTime(pid int) (time.Time, bool) {
	at, ok := f.parentStart[pid]

	return at, ok
}

// baseInspector is an ordinary runner: titles readable, other users' processes
// visible, not containerised.
func baseInspector() fakeInspector {
	return fakeInspector{titles: true, seesForeign: true, byPID: map[int]string{}}
}

// tcpFacts is a TCP connection whose backend the server reports at PID 163 from
// 172.19.0.3:35484, the shape measured on the fixture (§9).
func tcpFacts() sameHostFacts {
	return sameHostFacts{
		backendPID: "163",
		role:       "postgres",
		database:   "postgres",
		clientAddr: "172.19.0.3",
		clientPort: "35484",
		serverAddr: "172.19.0.2",
	}
}

func TestParseBackendTitle(t *testing.T) {
	tests := []struct {
		name  string
		title string
		want  parsedTitle
		ok    bool
	}{
		{
			name:  "TCP client, as measured on the fixture",
			title: "postgres: postgres postgres 172.19.0.3(35484) SELECT",
			want:  parsedTitle{role: "postgres", database: "postgres", addr: "172.19.0.3", port: "35484"},
			ok:    true,
		},
		{
			name:  "unix socket client",
			title: "postgres: postgres postgres [local] SELECT",
			want:  parsedTitle{role: "postgres", database: "postgres", local: true},
			ok:    true,
		},
		{
			name: "update_process_title=off keeps the fixed part",
			// Measured 2026-08-26 (§9): only the trailing activity stops updating,
			// which is why the match never reads it.
			title: "postgres: postgres postgres [local] ",
			want:  parsedTitle{role: "postgres", database: "postgres", local: true},
			ok:    true,
		},
		{
			name:  "cluster-name prefix, as Debian packaging writes it",
			title: "postgres: 17/main: ycrash_monitor orders_db 10.0.4.9(35484) idle",
			want: parsedTitle{
				role: "ycrash_monitor", database: "orders_db",
				addr: "10.0.4.9", port: "35484",
			},
			ok: true,
		},
		{
			name:  "a background worker is not a client backend",
			title: "postgres: checkpointer",
			ok:    false,
		},
		{
			name:  "an unrelated process at the same PID",
			title: "/usr/bin/containerd-shim-runc-v2 -namespace moby",
			ok:    false,
		},
		{
			name:  "a kernel thread reads as an empty command line",
			title: "",
			ok:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseBackendTitle(tt.title)
			require.Equal(t, tt.ok, ok)

			if tt.ok {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestSameHostYesFromTheTitleMatch(t *testing.T) {
	sys := baseInspector()
	sys.byPID[163] = "postgres: postgres postgres 172.19.0.3(35484) SELECT"

	got := checkSameHost(tcpFacts(), sys)

	assert.Equal(t, OnDBHostYes, got.verdict)
	assert.Equal(t, confirmedByBackendPID, got.by)
	assert.Empty(t, got.reason, "a yes carries no reason")
}

func TestSameHostRejectsAPIDCollisionOnUserAndDatabaseAlone(t *testing.T) {
	sys := baseInspector()
	sys.byPID[163] = "postgres: postgres postgres 127.0.0.1(51000) idle"

	got := checkSameHost(tcpFacts(), sys)

	require.False(t, sys.container, "not containerised, so the container check does not apply")
	assert.Equal(t, OnDBHostNo, got.verdict,
		"same role and database, different endpoint - a local cluster of the runner's own")
	assert.Equal(t, hostReasonTitleMismatch, got.reason)
}

func TestSameHostAcceptsALocalPoolerWhereBothSidesAgree(t *testing.T) {
	facts := tcpFacts()
	facts.clientAddr, facts.clientPort = "127.0.0.1", "6432"

	sys := baseInspector()
	sys.byPID[163] = "postgres: postgres postgres 127.0.0.1(6432) SELECT"

	assert.Equal(t, OnDBHostYes, checkSameHost(facts, sys).verdict)
}

func TestSameHostSocketTitleNeedsTheServerToAgreeItIsASocket(t *testing.T) {
	facts := tcpFacts()

	sys := baseInspector()
	sys.byPID[163] = "postgres: postgres postgres [local] SELECT"

	got := checkSameHost(facts, sys)

	assert.Equal(t, OnDBHostNo, got.verdict,
		"the server reported a TCP client, so a [local] title is somebody else's backend")
	assert.Equal(t, hostReasonTitleMismatch, got.reason)
}

func TestSameHostSocketCaseMatchesWhenBothSidesSaySocket(t *testing.T) {
	facts := tcpFacts()
	facts.clientAddr, facts.clientPort = "", ""

	sys := baseInspector()
	sys.byPID[163] = "postgres: postgres postgres [local] SELECT"

	assert.Equal(t, OnDBHostYes, checkSameHost(facts, sys).verdict)
}

func TestSameHostAbsentPID(t *testing.T) {
	tests := []struct {
		name        string
		seesForeign bool
		container   bool
		wantVerdict string
		wantReason  string
	}{
		{
			name:        "absent, foreign processes visible, no container - genuinely remote",
			seesForeign: true,
			wantVerdict: OnDBHostNo,
			wantReason:  hostReasonPIDAbsent,
		},
		{
			name:        "hidepid: absence proves nothing",
			seesForeign: false,
			wantVerdict: OnDBHostUnknown,
			wantReason:  hostReasonProcRestricted,
		},
		{
			name:        "the agent is in a container",
			seesForeign: true,
			container:   true,
			wantVerdict: OnDBHostUnknown,
			wantReason:  hostReasonContainer,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sys := baseInspector()
			sys.seesForeign, sys.container = tt.seesForeign, tt.container

			got := checkSameHost(tcpFacts(), sys)

			assert.Equal(t, tt.wantVerdict, got.verdict)
			assert.Equal(t, tt.wantReason, got.reason)
			assert.NotEmpty(t, got.reason, "a reason is mandatory whenever the verdict is not yes")
		})
	}
}

func TestSameHostFindsTheBackendThroughAPIDNamespace(t *testing.T) {
	sys := baseInspector()
	sys.byNamespacedPID = map[int]string{
		163: "postgres: postgres postgres 172.19.0.3(35484) SELECT",
	}

	got := checkSameHost(tcpFacts(), sys)

	assert.Equal(t, OnDBHostYes, got.verdict)
	assert.Equal(t, confirmedByBackendPIDNSpid, got.by)
}

func TestSameHostRejectsTheRootNamespaceCollisionAndStillFindsTheRealBackend(t *testing.T) {
	sys := baseInspector()
	sys.byPID[163] = ""
	sys.byNamespacedPID = map[int]string{
		163: "postgres: postgres postgres 172.19.0.3(35484) SELECT",
	}

	got := checkSameHost(tcpFacts(), sys)

	assert.Equal(t, OnDBHostYes, got.verdict)
	assert.Equal(t, confirmedByBackendPIDNSpid, got.by,
		"the bare PID was visible but empty; the namespace scan found the real one")
}

func TestSameHostContainerisedAgentWithAForeignTitleIsUnknownNotNo(t *testing.T) {
	sys := baseInspector()
	sys.container = true
	sys.byPID[163] = "/usr/local/bin/some-sidecar --serve"

	got := checkSameHost(tcpFacts(), sys)

	assert.Equal(t, OnDBHostUnknown, got.verdict)
	assert.Equal(t, hostReasonContainer, got.reason)
}

func TestSameHostManagedServiceIsDecisiveAndOutranksEverything(t *testing.T) {
	facts := tcpFacts()
	facts.managedService = true

	sys := baseInspector()
	sys.byPID[163] = "postgres: postgres postgres 172.19.0.3(35484) SELECT"

	got := checkSameHost(facts, sys)

	assert.Equal(t, OnDBHostNo, got.verdict,
		"a managed database is on no machine the agent could occupy, whatever the local table says")
	assert.Equal(t, hostReasonManagedService, got.reason)
}

func TestSameHostUnreadBackendPID(t *testing.T) {
	facts := tcpFacts()
	facts.backendPID = ""

	got := checkSameHost(facts, baseInspector())

	assert.Equal(t, OnDBHostUnknown, got.verdict)
	assert.Equal(t, hostReasonBackendPIDUnread, got.reason)
}

func TestSameHostTitleFreeStartTimeTest(t *testing.T) {
	start := time.Date(2026, 8, 26, 9, 41, 23, 0, time.UTC)

	facts := tcpFacts()
	facts.postmasterStart = start.Format(timestampLayout)

	t.Run("within tolerance is a yes", func(t *testing.T) {
		sys := baseInspector()
		sys.titles = false
		sys.parentStart = map[int]time.Time{163: start.Add(-time.Second)}

		got := checkSameHost(facts, sys)

		assert.Equal(t, OnDBHostYes, got.verdict)
		assert.Equal(t, confirmedByPostmasterStart, got.by)
	})

	t.Run("outside tolerance is unknown, never no", func(t *testing.T) {
		sys := baseInspector()
		sys.titles = false
		sys.parentStart = map[int]time.Time{163: start.Add(time.Hour)}

		got := checkSameHost(facts, sys)

		assert.Equal(t, OnDBHostUnknown, got.verdict,
			"a disagreeing clock is not positive evidence of absence")
		assert.Equal(t, hostReasonPlatformNoTitles, got.reason)
	})

	t.Run("no parent reading at all is unknown", func(t *testing.T) {
		sys := baseInspector()
		sys.titles = false

		got := checkSameHost(facts, sys)

		assert.Equal(t, OnDBHostUnknown, got.verdict)
		assert.Equal(t, hostReasonPlatformNoTitles, got.reason)
	})
}

func TestSameHostEvidenceIsRecordedNotActedOn(t *testing.T) {
	facts := tcpFacts()
	facts.dialedSocket = true
	facts.logDirect = true
	facts.serverAddr = ""

	sys := baseInspector()

	got := checkSameHost(facts, sys)

	assert.Equal(t, []string{evidenceClientSocket, evidenceInetServerNull, evidenceLogFile},
		got.evidence, "sorted, so the row is stable across runs")
	assert.Equal(t, OnDBHostNo, got.verdict,
		"three supporting signals and still a no: evidence never moves the verdict")
}

func TestSameHostAlwaysPairsANonYesWithAReason(t *testing.T) {
	facts := tcpFacts()

	for _, sys := range []fakeInspector{
		baseInspector(),
		{titles: true, seesForeign: false},
		{titles: true, seesForeign: true, container: true},
		{titles: false},
		{titles: true, seesForeign: true, byPID: map[int]string{163: "/usr/sbin/sshd -D"}},
	} {
		got := checkSameHost(facts, sys)

		if got.verdict == OnDBHostYes {
			assert.Empty(t, got.reason)

			continue
		}

		assert.NotEmpty(t, got.reason, "verdict %q carried no reason", got.verdict)
		assert.Empty(t, got.by, "only a yes names the test that produced it")
	}
}
