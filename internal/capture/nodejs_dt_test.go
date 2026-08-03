package capture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestNodeDataTypeConstants(t *testing.T) {
	// Must match YCrashDataType.java's NODEJS_* entries' text (dt=) values
	// exactly - tier1app classifies uploads by an exact string match, so any
	// drift here silently drops that artifact on upload.
	want := map[string]string{
		"nodeDTProcessOverview":     "nodepo",
		"nodeDTEventLoopLag":        "nodeell",
		"nodeDTUnhandledRejections": "nodeur",
		"nodeDTModuleInventory":     "nodemi",
		"nodeDTHandleGrowth":        "nodehg",
		"nodeDTGCStats":             "nodegcs",
		"nodeDTWorkerCPUProfiles":   "nodewcpu",
	}
	got := map[string]string{
		"nodeDTProcessOverview":     nodeDTProcessOverview,
		"nodeDTEventLoopLag":        nodeDTEventLoopLag,
		"nodeDTUnhandledRejections": nodeDTUnhandledRejections,
		"nodeDTModuleInventory":     nodeDTModuleInventory,
		"nodeDTHandleGrowth":        nodeDTHandleGrowth,
		"nodeDTGCStats":             nodeDTGCStats,
		"nodeDTWorkerCPUProfiles":   nodeDTWorkerCPUProfiles,
	}
	for name, wantVal := range want {
		if got[name] != wantVal {
			t.Errorf("%s = %q, want %q", name, got[name], wantVal)
		}
	}

	// No Node dt= code may collide with the Java/.NET thread-dump code ("td") or
	// the GC-log code ("gc").
	for name, val := range got {
		if val == "td" {
			t.Errorf("%s = %q collides with the Java/.NET THREAD_DUMP dt=td", name, val)
		}
		if val == "gc" {
			t.Errorf("%s = %q collides with the GC-log dt=gc", name, val)
		}
	}

	// Explicit, mirroring the comment on the constant: GC stats must never be "gc".
	if nodeDTGCStats == "gc" {
		t.Errorf("nodeDTGCStats must not be \"gc\" (would collide with the real GC log)")
	}

	// All Node dt= codes must be distinct from one another.
	seen := map[string]string{}
	for name, val := range got {
		if prev, dup := seen[val]; dup {
			t.Errorf("dt code %q used by both %s and %s", val, prev, name)
		}
		seen[val] = name
	}
}

func TestNodeUploadCallSitesNeverUseTd(t *testing.T) {
	f := filepath.Join(repoRoot(t), "internal", "capture", "nodejs_capture.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, f, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", f, err)
	}

	uploaders := map[string]bool{
		"PostData": true, "PostDataWithTimeout": true, "PostReaderWithTimeout": true,
		"PostCustomData": true, "PostCustomDataWithTimeout": true,
	}
	sawUpload := false
	gcLiteralCount := 0
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var fn string
		switch fe := call.Fun.(type) {
		case *ast.Ident:
			fn = fe.Name
		case *ast.SelectorExpr:
			fn = fe.Sel.Name
		}
		if !uploaders[fn] {
			return true
		}
		sawUpload = true
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue // constants (nodeDT*) and concatenations are checked elsewhere
			}
			val, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				continue
			}
			if val == "td" {
				t.Errorf("%s(...) passes literal dt %q — Node data must never upload as dt=td", fn, val)
			}
			if val == "gc" {
				gcLiteralCount++
			}
		}
		return true
	})

	if !sawUpload {
		t.Fatal("no upload calls found in nodejs_capture.go; wiring moved or scan is broken")
	}
	if gcLiteralCount != 1 {
		t.Errorf("literal dt=\"gc\" appears in %d upload call(s) in nodejs_capture.go, want exactly 1 (the GC log)", gcLiteralCount)
	}
}

// TestNodeAppLogFileNameMatchesBundleConvention guards against a repeat of a
// real bug: NodeAppLogFileName used to be a bare "nodeapp.log", which
// YCrashDataType.fromAgentFileName() in tier1app can't classify as an app log
// (it only recognizes an exact "applog.out" match or a filename containing
// ".appLogs.", the marker the generic Java/.NET app-log capture already uses
// - see internal/capture/applog.go's generateUniqueLogPath). Since
// PostData/PostCustomData never actually uploads anything in -onlyCapture
// mode (see post.go's OnlyCapture short-circuit), filename-based bundle
// classification was the ONLY path this artifact could reach tier1app
// through, and it silently failed every time.
func TestNodeAppLogFileNameMatchesBundleConvention(t *testing.T) {
	if !strings.Contains(NodeAppLogFileName, ".appLogs.") {
		t.Errorf("NodeAppLogFileName = %q must contain \".appLogs.\" so tier1app's "+
			"BundleUploadServlet can classify it as an app log when uploaded via -onlyCapture "+
			"(YCrashDataType.fromAgentFileName only recognizes an exact \"applog.out\" match "+
			"or a filename containing \".appLogs.\")", NodeAppLogFileName)
	}

	// The real-time applog upload (on-demand/M3) dispatches server-side on the
	// explicit dt=applog param, not the filename - so its logName should stay a
	// clean, human-readable name, not carry the bundle-only ".appLogs." marker.
	if NodeAppLogDisplayName != "nodeapp.log" {
		t.Errorf("NodeAppLogDisplayName = %q, want \"nodeapp.log\"", NodeAppLogDisplayName)
	}
	if strings.Contains(NodeAppLogDisplayName, ".appLogs.") {
		t.Errorf("NodeAppLogDisplayName = %q must not carry the bundle-only \".appLogs.\" marker - "+
			"it's sent as logName= on the real-time upload path, not used for bundle classification", NodeAppLogDisplayName)
	}
}

func TestNodeGCStatsStaysUnwired(t *testing.T) {
	agentDir := filepath.Join(repoRoot(t), "internal", "agent")

	found := false
	sawProcessOverview := false
	walkErr := filepath.WalkDir(agentDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := cl.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "capture" {
				switch sel.Sel.Name {
				case "NodeGCStats":
					found = true
				case "NodeProcessOverview":
					sawProcessOverview = true
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", agentDir, walkErr)
	}

	// Sanity anchor covering the WHOLE tree: if we found no NodeProcessOverview
	// construction anywhere, the scan is broken/mislocated and any "unwired" pass
	// would be vacuous.
	if !sawProcessOverview {
		t.Fatal("no capture.NodeProcessOverview{} construction found under internal/agent; wiring moved or scan is broken")
	}
	if found {
		t.Error("capture.NodeGCStats{} is constructed under internal/agent — it must stay unwired (would emit dt=nodegcs)")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate go.mod above %s", dir)
		}
		dir = parent
	}
}
