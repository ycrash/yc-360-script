package m3

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"yc-agent/internal/config"
)

func TestCaptureNodeM3TaskSet(t *testing.T) {
	m3go := filepath.Join(moduleRoot(t), "internal", "agent", "m3", "m3.go")
	fn := findFuncDecl(t, m3go, "captureNodeM3")

	// Collect every capture.<Type>{...} composite literal constructed in the body.
	constructed := map[string]*ast.CompositeLit{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if sel, ok := cl.Type.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "capture" {
				constructed[sel.Sel.Name] = cl
			}
		}
		return true
	})

	// Exactly the three lightweight tasks — no more, no fewer.
	want := map[string]bool{
		"NodeGC":              true,
		"NodeProcessOverview": true,
		"NodeHeapSummary":     true,
	}
	for name := range want {
		if _, ok := constructed[name]; !ok {
			t.Errorf("captureNodeM3 no longer constructs capture.%s", name)
		}
	}
	forbidden := []string{
		"NodeCPUProfile", "NodeWorkerCPUProfiles", "NodeEventLoopLag", "NodeUnhandledRejections",
		"NodeModuleInventory", "NodeHandleGrowth", "NodeGCStats",
	}
	for _, name := range forbidden {
		if _, ok := constructed[name]; ok {
			t.Errorf("captureNodeM3 constructs capture.%s — heavy/windowed tasks must stay out of the M3 cycle", name)
		}
	}
	for name := range constructed {
		if !want[name] {
			t.Errorf("captureNodeM3 constructs an unexpected capture.%s; the M3 cycle must stay limited to the 3 lightweight tasks", name)
		}
	}

	// NodeGC must be built for M3 mode: M3:true (continuous delta only, no dumpGC
	// fallback) and a non-nil Tracker (per-(pid,file) incremental offset). Assert
	// the VALUES, not just key presence — M3:false would silently re-enable the
	// dumpGC fallback, and Tracker:nil would re-upload the whole stdout each cycle.
	gc := constructed["NodeGC"]
	if gc == nil {
		t.Fatal("NodeGC not constructed")
	}
	fields := compositeLitFields(gc)
	if v, ok := fields["M3"]; !ok || !isIdent(v, "true") {
		t.Errorf("NodeGC in captureNodeM3 must set M3: true (continuous delta only, no dumpGC fallback); got %s", exprString(v))
	}
	if v, ok := fields["Tracker"]; !ok || isIdent(v, "nil") {
		t.Errorf("NodeGC in captureNodeM3 must set a non-nil Tracker (M3 incremental delta reads); got %s", exprString(v))
	}
}

func TestCaptureNodeM3Subfolder(t *testing.T) {
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })
	config.GlobalConfig.NodejsRuntimeDir = t.TempDir() // empty dir → no hook registration → tasks no-op
	config.GlobalConfig.OnlyCapture = true             // belt-and-suspenders: no HTTP upload

	// captureNodeM3 writes to a RELATIVE dir (yc-node-m3/<pid>); the real M3 flow
	// runs inside a per-cycle capture dir, so mimic that with a temp CWD.
	tmp := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWd) })

	app := NewM3App()
	t.Cleanup(app.Shutdown)

	pid := os.Getpid()
	stdoutPath := app.captureNodeM3("http://127.0.0.1:0?de=test", pid)

	// Our own test process wasn't started with --trace-gc, so no stdout path.
	if stdoutPath != "" {
		t.Errorf("expected empty stdout path for a non--trace-gc process, got %q", stdoutPath)
	}
	// The per-PID subfolder must exist.
	want := filepath.Join("yc-node-m3", strconv.Itoa(pid))
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("expected per-PID subfolder %s to be created (err=%v)", want, err)
	}
}

// ---------------------------------------------------------------------------
// AST helpers.
// ---------------------------------------------------------------------------

func findFuncDecl(t *testing.T, absPath, name string) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, absPath, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", absPath, err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}
	t.Fatalf("function %s not found in %s", name, absPath)
	return nil
}

func compositeLitFields(cl *ast.CompositeLit) map[string]ast.Expr {
	fields := map[string]ast.Expr{}
	for _, elt := range cl.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok {
				fields[id.Name] = kv.Value
			}
		}
	}
	return fields
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case nil:
		return "<absent>"
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	default:
		return fmt.Sprintf("%T", e)
	}
}

func moduleRoot(t *testing.T) string {
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
