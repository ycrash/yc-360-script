package executils

import (
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestCopy2TempPath(t *testing.T) {
	tmpPathMtx.Lock()
	tmpPath = ""
	tmpPathMtx.Unlock()
	t.Cleanup(RemoveFromTempPath)

	// Concurrent fallbacks must share a single copy, otherwise extra copies
	// are leaked in the temp dir.
	const callers = 8
	names := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			names[i], errs[i] = Copy2TempPath()
		}(i)
	}
	wg.Wait()

	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("Copy2TempPath returned error: %v", errs[i])
		}
		if names[i] != names[0] {
			t.Fatalf("expected one shared temp copy, got %q and %q", names[0], names[i])
		}
	}

	// Windows only runs files with an executable extension.
	if runtime.GOOS == "windows" && !strings.HasSuffix(strings.ToLower(names[0]), ".exe") {
		t.Fatalf("temp copy %q must end in .exe on Windows", names[0])
	}

	if _, err := os.Stat(names[0]); err != nil {
		t.Fatalf("temp copy not found: %v", err)
	}

	RemoveFromTempPath()
	if _, err := os.Stat(names[0]); !os.IsNotExist(err) {
		t.Fatalf("expected temp copy to be removed, stat err = %v", err)
	}

	// After cleanup, a new call must make a fresh copy, not return the deleted path.
	again, err := Copy2TempPath()
	if err != nil {
		t.Fatalf("Copy2TempPath after cleanup returned error: %v", err)
	}
	if _, err := os.Stat(again); err != nil {
		t.Fatalf("Copy2TempPath after cleanup returned a missing file %q: %v", again, err)
	}
}
