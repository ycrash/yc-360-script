package shell

var (
	NetState            = Command{"netstat", "-an"}
	PS                  = Command{"ps", "-ef"}
	PS2                 = Command{"ps", "-ef"}
	M3PS                = Command{"ps", "-ef"}
	Disk                = Command{"df", "-hk"}
	Top                 = Command{WaitCommand, "/bin/sh", "-c", "for i in {1..3}; do top -l 1 ; sleep 10; done"}
	Top2                = NopCommand
	TopH                = Command{WaitCommand, "top", "-l", "1", "-pid", DynamicArg}
	TopH2               = NopCommand
	Top4M3              = Command{"top", "-l", "1"}
	VMState             = Command{"/bin/sh", "-c", "vm_stat -c 10 5"}
	DMesg               = Command{"dmesg"}
	DMesg2              = Command{"/bin/sh", "-c", "cat /var/log/system.log | tail -20"}
	GC                  = Command{"ps", "-f", "-p", DynamicArg}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"/bin/sh", "-c", "cat topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{"ps", "-eo", "pid,command,%cpu", "-r"}
	ProcessTopMEM       = Command{"ps", "-eo", "pid,command,%mem", "-m"}
	OSVersion           = Command{WaitCommand, "uname", "-a"}
	KernelParam         = Command{WaitCommand, "sysctl", "-a"}
	Ping                = Command{WaitCommand, "ping", "-c", "6"}
	JavaVersionCommand  = Command{"java", "-XshowSettings:java", "-version"}

	SHELL = Command{"/bin/sh", "-c"}
)
