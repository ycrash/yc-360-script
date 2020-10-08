package shell

var (
	NetState = Command{"netstat", "-pan"}
	PS       = Command{"ps", "-eLf"}
	Disk     = Command{"df", "-hk"}
	Top      = Command{"top", "-bc",
		"-d", strconv.Itoa(shell.TOP_INTERVAL),
		"-n", strconv.Itoa(shell.SCRIPT_SPAN/shell.TOP_INTERVAL + 1)}
	TopH = Command{"top", "-bH",
		"-d", strconv.Itoa(shell.TOP_DASH_H_INTERVAL),
		"-n", strconv.Itoa(shell.SCRIPT_SPAN/shell.TOP_DASH_H_INTERVAL + 1),
		"-p", strconv.Itoa(t.Pid)}
	VMState             = Command{"vmstat", DynamicArg, DynamicArg, `| awk '{now=strftime("%T "); print now $0; fflush()}'`}
	DMesg               = Command{"dmesg"}
	GC                  = Command{"/bin/sh", "-c"}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}

	SHELL = Command{"/bin/sh", "-c"}
)
