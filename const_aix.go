package shell

var (
	NetState            = Command{"netstat", "-a"}
	PS                  = Command{"ps", "-ef"}
	M3PS                = Command{"ps", "-ef"}
	Disk                = Command{"df"}
	Top                 = Command{"topas", "-P"}
	TopH                = Command{"topas", "-P"}
	VMState             = Command{"vmstat", DynamicArg, DynamicArg}
	DMesg               = Command{"dmesg"}
	GC                  = Command{"/bin/sh", "-c"}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"/bin/sh", "-c", "cat topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{"ps", "-eo", "pid,cmd,%cpu", "--sort=-%cpu"}
	ProcessTopMEM       = Command{"ps", "-eo", "pid,cmd,%mem", "--sort=-%mem"}

	SHELL = Command{"/bin/sh", "-c"}
)
