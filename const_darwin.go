package shell

var (
	NetState            = Command{"netstat", "-pan"}
	PS                  = Command{"ps", "-eLf"}
	Disk                = Command{"df", "-hk"}
	Top                 = Command{"top", "-bc"}
	TopH                = Command{"top", "-bH"}
	VMState             = Command{"vmstat"}
	DMesg               = Command{"dmesg"}
	GC                  = Command{"/bin/sh", "-c"}
	AppendJavaCoreFiles = Command{"/bin/sh", "-c", "cat javacore.* > threaddump.out"}
	AppendTopH          = Command{"/bin/sh", "-c"}

	shell = Command{"/bin/sh", "-c"}
)
