package executils

var (
	NetState            = Command{"netstat", "-a"}
	PS                  = Command{"ps", "-ef"}
	PS2                 = Command{"ps", "-ef"}
	M3PS                = Command{"ps", "-ef"}
	Disk                = Command{"df"}
	Top                 = Command{"topas", "-P"}
	Top2                = Command{"topas", "-P"}
	TopH                = Command{"topas", "-P"}
	TopH2               = Command{"topas", "-P"}
	Top4M3              = Command{"topas", "-P"}
	VMState             = Command{"vmstat", DynamicArg, DynamicArg}
	DMesg               = Command{"dmesg"}
	DMesg2              = Command{"dmesg"}
	GC                  = Command{"ps", "-f", "-p", DynamicArg}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"/bin/sh", "-c", "cat topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{"ps", "-eo", "pid,cmd,%cpu", "--sort=-%cpu"}
	ProcessTopMEM       = Command{"ps", "-eo", "pid,cmd,%mem", "--sort=-%mem"}
	OSVersion           = Command{WaitCommand, "uname", "-a"}
	KernelParam         = Command{WaitCommand, "sysctl", "-a"}
	JavaVersionCommand  = Command{"java", "-XshowSettings:java", "-version"}

	SHELL = Command{"/bin/sh", "-c"}
)
