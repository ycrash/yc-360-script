package capture

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProcessLogFile(t *testing.T) {
	fatalIf := func(err error) {
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	fatalIf(os.Remove("gc-rotation-logs/0-current/1/gc.log"))
	fatalIf(os.Remove("gc-rotation-logs/0-current/2/gc.log"))
	fatalIf(os.Remove("gc-rotation-logs/0-current/3/gc.log"))
	fatalIf(os.Remove("gc-rotation-logs/1-current/gc.log"))
	fatalIf(os.Remove("gc-rotation-logs/2-current/gc.log"))
	test := func(t *testing.T, dir string, fname string, out string) {
		if len(out) < 1 {
			out = fname
		}
		gc, err := ProcessGCLogFile(filepath.Join(dir, fname), filepath.Join(dir, out), "", 0)
		if err != nil {
			t.Fatal(err)
		}
		gc.Seek(0, 0)
		all, err := io.ReadAll(gc)
		if err != nil {
			t.Fatal(err)
		}
		s := string(all)
		if s != "test\ntest" {
			t.Fatal(s)
		}
	}
	t.Run("0-current-1", func(t *testing.T) {
		dir := "gc-rotation-logs/0-current/1"
		test(t, dir, "gc.log", "")
	})
	t.Run("0-current-2", func(t *testing.T) {
		dir := "gc-rotation-logs/0-current/2"
		test(t, dir, "gc.log", "")
	})
	t.Run("0-current-3", func(t *testing.T) {
		dir := "gc-rotation-logs/0-current/3"
		test(t, dir, "gc.log", "")
	})
	t.Run("1-current", func(t *testing.T) {
		dir := "gc-rotation-logs/1-current"
		test(t, dir, "gc.log", "")
	})
	t.Run("2-current", func(t *testing.T) {
		dir := "gc-rotation-logs/2-current"
		test(t, dir, "gc.log", "")
	})

	fatalIf(os.Remove("gc-rotation-logs/0-current/1/gc.log"))
	t.Run("gcPath-exists", func(t *testing.T) {
		dir := "gc-rotation-logs/0-current/1"
		test(t, dir, "gc.log.0.current", "gc.log")
	})
	t.Run("gcPath-not-exists", func(t *testing.T) {
		_, err := ProcessGCLogFile("gc-rotation-logs/0-current/1/gc.log.current", "gc-rotation-logs/0-current/1/gc.log", "", 0)
		if err != nil && errors.Is(err, os.ErrNotExist) && strings.Contains(err.Error(), "can not find the current log file,") {
		} else {
			t.Fatal(err)
		}
	})

	// https://tier1app.atlassian.net/browse/GCEA-2339
	t.Run("gc%t", func(t *testing.T) {
		dir := "gc%t"
		test(t, dir, "gc%t.log", "gctt.log")
	})
}
