package api

import (
	"errors"
	"os"
	"strconv"
	"testing"

	"yc-agent/internal/config"
)

func TestActionAPIExplicitPidReachesProcessPids(t *testing.T) {
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })
	config.GlobalConfig.ApiKey = "test-key"

	s := NewServer("localhost", 0)
	var gotPids []int
	s.ProcessPids = func(pids []int, pid2Name map[int]string, hd bool, tags string) ([]string, error) {
		gotPids = append(gotPids, pids...)
		return []string{"http://dashboard/report/1"}, nil
	}

	pid := os.Getpid() // a guaranteed-existing PID (validateActionAPIParseResult requires one)
	req := &ActionRequest{
		Key:     "test-key",
		Actions: []string{"capture " + strconv.Itoa(pid)},
		WaitFor: true, // run synchronously so the assertion sees the result
	}
	resp := &ActionResponse{}
	s.handleActionAPI(req, resp)

	if len(gotPids) != 1 || gotPids[0] != pid {
		t.Errorf("ProcessPids received %v, want [%d]", gotPids, pid)
	}
	if len(resp.DashboardReportURLs) != 1 || resp.DashboardReportURLs[0] != "http://dashboard/report/1" {
		t.Errorf("DashboardReportURLs = %v, want one report URL", resp.DashboardReportURLs)
	}
}

func TestActionAPIProcessPidsErrorSurfaced(t *testing.T) {
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })
	config.GlobalConfig.ApiKey = "test-key"

	s := NewServer("localhost", 0)
	s.ProcessPids = func(pids []int, pid2Name map[int]string, hd bool, tags string) ([]string, error) {
		return nil, errors.New("capture blew up")
	}

	req := &ActionRequest{
		Key:     "test-key",
		Actions: []string{"capture " + strconv.Itoa(os.Getpid())},
		WaitFor: true,
	}
	resp := &ActionResponse{}
	s.handleActionAPI(req, resp) // must not panic (goroutine nil-deref before the fix)

	if resp.Code != -1 {
		t.Errorf("resp.Code = %d, want -1 on a capture error", resp.Code)
	}
	if resp.Msg != "capture blew up" {
		t.Errorf("resp.Msg = %q, want the ProcessPids error surfaced verbatim", resp.Msg)
	}
}

func TestActionAPIRejectsBadKey(t *testing.T) {
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })
	config.GlobalConfig.ApiKey = "right-key"

	s := NewServer("localhost", 0)
	called := false
	s.ProcessPids = func(pids []int, pid2Name map[int]string, hd bool, tags string) ([]string, error) {
		called = true
		return nil, nil
	}
	resp := &ActionResponse{}
	s.handleActionAPI(&ActionRequest{Key: "wrong-key", Actions: []string{"capture 1"}, WaitFor: true}, resp)

	if called {
		t.Errorf("ProcessPids must not run when the API key is wrong")
	}
	if resp.Code != -1 || resp.Msg != "invalid key passed" {
		t.Errorf("resp = %+v, want Code=-1 Msg=\"invalid key passed\"", resp)
	}
}

func TestActionAPIParseActionsDispatch(t *testing.T) {
	// A numeric id parses straight to that PID (runtime-agnostic — a Node PID
	// works exactly like a Java one here).
	result, _, _, err := parseActions([]string{"capture 424242"})
	if err != nil {
		t.Fatalf("parseActions numeric: %v", err)
	}
	if len(result) != 1 || result[0] != 424242 {
		t.Errorf("numeric dispatch = %v, want [424242]", result)
	}

	// A non-numeric token is resolved by command-line substring match via
	// GetProcessIds — the runtime-agnostic path by which a Node process CAN be
	// targeted (with a unique token). A sentinel that matches no process yields
	// nothing.
	result, _, _, err = parseActions([]string{"capture zzz_no_such_process_token_zzz"})
	if err != nil {
		t.Fatalf("parseActions token: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("unmatched token dispatch = %v, want empty", result)
	}

	if _, _, _, err := parseActions([]string{"capture PROCESS_HIGH_CPU"}); err != nil {
		t.Fatalf("parseActions PROCESS_HIGH_CPU dispatch errored: %v", err)
	}
}
