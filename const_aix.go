package shell

var (
	NetState            = Command{"netstat", "-a"}
	PS                  = Command{"ps", "-ef"}
	Disk                = Command{"df"}
	Top                 = Command{"topas", "-P"}
	TopH                = Command{"topas", "-P"}
	VMState             = Command{"vmstat", DynamicArg, DynamicArg}
	DMesg               = Command{"dmesg"}
	GC                  = Command{"/bin/sh", "-c"}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}

	SHELL = Command{"/bin/sh", "-c"}
)
