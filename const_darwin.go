package shell

var (
	NetState            = Command{"netstat", "-pan"}
	PS                  = Command{"ps", "-ef"}
	PS2                 = Command{"ps", "-ef"}
	M3PS                = Command{"ps", "-ef"}
	Disk                = Command{"df", "-hk"}
	Top                 = Command{"top", "-bc"}
	Top2                = Command{"top", "-bc"}
	TopH                = Command{WaitCommand, "top", "-l", "1", "-pid", DynamicArg}
	TopH2               = Command{WaitCommand, "top", "-l", "1", "-pid", DynamicArg}
	Top4M3              = Command{"top", "-bc"}
	VMState             = Command{"vm_stat"}
	DMesg               = Command{"dmesg"}
	DMesg2              = Command{"/bin/sh", "-c", "cat /var/log/system.log | tail -20"}
	GC                  = Command{"/bin/sh", "-c"}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"/bin/sh", "-c", "cat topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{"ps", "-eo", "pid,command,%cpu", "-r"}
	ProcessTopMEM       = Command{"ps", "-eo", "pid,command,%mem", "-m"}
	OSVersion           = Command{WaitCommand, "uname", "-a"}
	KernelParam         = Command{WaitCommand, "sysctl", "-a"}
	Ping                = Command{WaitCommand, "ping", "-c", "6"}

	SHELL = Command{"/bin/sh", "-c"}
)
