package shell

var (
	NetState = Command{"netstat", "-pan"}
	PS       = Command{"ps", "-eLf"}
	M3PS     = Command{"ps", "-eLf"}
	Disk     = Command{"df", "-hk"}
	Top      = Command{WaitCommand, "top", "-bc",
		"-d", "10",
		"-n", "3"}
	TopH = Command{WaitCommand, "top", "-bH",
		"-n", "1",
		"-p", DynamicArg}
	Top4M3              = Command{WaitCommand, "top", "-bc", "-n", "1"}
	VMState             = Command{WaitCommand, "vmstat", DynamicArg, DynamicArg, `| awk '{now=strftime("%T "); print now $0; fflush()}'`}
	DMesg               = Command{"/bin/sh", "-c", "dmesg -T --level=emerg,alert,crit,err,warn | tail -20"}
	GC                  = Command{"/bin/sh", "-c"}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"/bin/sh", "-c", "cat topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{"/bin/sh", "-c", "ps -o pid,%cpu,cmd, ax | sort -b -k2 -r"}
	ProcessTopMEM       = Command{"/bin/sh", "-c", "ps -o pid,%mem,cmd, ax | sort -b -k2 -r"}

	SHELL = Command{"/bin/sh", "-c"}
)
