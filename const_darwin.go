package shell

var (
	NetState            = Command{"netstat", "-pan"}
	PS                  = Command{"ps", "-ef"}
	Disk                = Command{"df", "-hk"}
	Top                 = Command{"top", "-bc"}
	TopH                = Command{WaitCommand, "top", "-l", "1", "-pid", DynamicArg}
	VMState             = Command{"vmstat"}
	DMesg               = Command{"dmesg"}
	GC                  = Command{"/bin/sh", "-c"}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"/bin/sh", "-c", "cat topdashH.* >> threaddump.out"}

	SHELL = Command{"/bin/sh", "-c"}
)
