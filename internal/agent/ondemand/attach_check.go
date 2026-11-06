package ondemand

import (
	"fmt"
	"strings"

	"yc-agent/internal/logger"
)

const (
	disableAttachFlag = "-XX:+DisableAttachMechanism"
	enableAttachFlag  = "-XX:-DisableAttachMechanism"
)

// readAttachCmdline reads the target's command line; a variable so tests can
// stub it.
var readAttachCmdline = cmdlineFromGopsutil

// attachDisabledByFlag reports whether a JVM command line turns off the attach
// mechanism. When the flag is given more than once, the last one wins, as in
// the JVM.
func attachDisabledByFlag(cmdline string) bool {
	return strings.LastIndex(cmdline, disableAttachFlag) > strings.LastIndex(cmdline, enableAttachFlag)
}

// attachCaptureOptions are the settings that decide which captures attach
// to the JVM in this run.
type attachCaptureOptions struct {
	tdPath       string // thread dumps come from this file instead
	hdPath       string // the heap dump comes from this file instead
	heapDump     bool   // -hd
	jfr          bool   // -jfrEnabled
	minimalTouch bool   // -minimalTouch skips JFR, heap dump, histogram and system properties
}

// attachCaptureNames lists the captures in this run that attach to the JVM,
// and so fail when attach is disabled.
func attachCaptureNames(o attachCaptureOptions) []string {
	var names []string
	if o.tdPath == "" {
		names = append(names, "thread dumps")
	}
	if !o.minimalTouch {
		if o.jfr {
			names = append(names, "JFR")
		}
		if o.heapDump && o.hdPath == "" {
			names = append(names, "heap dump")
		}
		names = append(names, "class histogram", "system properties")
	}
	return names
}

// warnIfAttachDisabled logs one warning up front when the JVM was started
// with -XX:+DisableAttachMechanism. Every capture that attaches to the JVM
// (jcmd, jattach, jstack) then fails, and without this warning the cause is
// buried in their error messages. Captures still run as before. A flag set
// through JAVA_TOOL_OPTIONS or an @argfile isn't on the command line, so it
// isn't detected here.
func warnIfAttachDisabled(pid int, o attachCaptureOptions) {
	names := attachCaptureNames(o)
	if len(names) == 0 {
		return // nothing in this run attaches to the JVM
	}
	cmdline, err := readAttachCmdline(pid)
	if err != nil || !attachDisabledByFlag(string(cmdline)) {
		return
	}
	logger.Warn().Msg(attachDisabledMessage(pid, names, o.minimalTouch))
}

func attachDisabledMessage(pid int, names []string, minimalTouch bool) string {
	stillCaptured := "GC data and host data are still captured."
	if minimalTouch {
		// -minimalTouch skips jstat, so GC data only comes from a GC log file.
		stillCaptured = "Host data, and GC data from the JVM's GC log file if it writes one, are still captured."
	}
	return fmt.Sprintf("PID %d was started with %s, so the yc-360 script can't attach to it: %s will fail. %s To capture everything, remove %s and restart the application.",
		pid, disableAttachFlag, joinWithAnd(names), stillCaptured, disableAttachFlag)
}

// joinWithAnd joins names as "a, b and c".
func joinWithAnd(names []string) string {
	if len(names) <= 1 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
