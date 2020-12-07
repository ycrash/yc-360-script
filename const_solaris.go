package shell

import (
	"strconv"
)

var (
	NetState = Command{"netstat", "-pan"}
	PS       = Command{"ps", "-eLf"}
	M3PS     = Command{"ps", "-eLf"}
	Disk     = Command{"df", "-hk"}
	Top      = Command{WaitCommand, "top", "-bc",
		"-d", strconv.Itoa(TOP_INTERVAL),
		"-n", strconv.Itoa(SCRIPT_SPAN/TOP_INTERVAL + 1)}
	TopH = Command{WaitCommand, "top", "-bH",
		"-n", "1",
		"-p", DynamicArg}
	Top4M3              = Command{WaitCommand, "top", "-bc", "-n", "1"}
	VMState             = Command{WaitCommand, "vmstat", DynamicArg, DynamicArg, `| awk '{now=strftime("%T "); print now $0; fflush()}'`}
	DMesg               = Command{"dmesg"}
	GC                  = Command{"/bin/sh", "-c"}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"/bin/sh", "-c", "cat topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{"ps", "-eo", "pid,cmd,%cpu", "--sort=-%cpu"}
	ProcessTopMEM       = Command{"ps", "-eo", "pid,cmd,%mem", "--sort=-%mem"}

	SHELL = Command{"/bin/sh", "-c"}
)
