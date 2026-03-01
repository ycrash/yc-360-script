package executils

var (
	NetState            = Command{"netstat", "-a"}
	PS                  = Command{"ps", "-ef"}
	PS2                 = Command{"ps", "-ef"}
	PSGetProcessIds     = Command{"ps", "-ef"}
	Disk                = Command{"df"}
	Top                 = Command{WaitCommand, "nmon", "-F", "/tmp/yc_nmon_top.csv", "-s", "10", "-c", "3", "-T", "&&", "cat", "/tmp/yc_nmon_top.csv"}
	Top2                = Command{WaitCommand, "nmon", "-F", "/tmp/yc_nmon_top.csv", "-s", "10", "-c", "3", "-T", "&&", "cat", "/tmp/yc_nmon_top.csv"}
	TopH                = Command{WaitCommand, "ps", "-mp", DynamicArg, "-o", "THREAD"}
	TopH2               = Command{WaitCommand, "ps", "-mp"}
	Top4M3              = Command{WaitCommand, "ps", "-eo", "pid,pcpu,pmem,args"}
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
	Ping                = Command{WaitCommand, "ping", "-c", "6"}

	SHELL = Command{"/bin/sh", "-c"}
)
