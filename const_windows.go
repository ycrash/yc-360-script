package shell

var (
	NetState            = Command{"netstat", "-an"}
	PS                  = Command{"tasklist"}
	Disk                = Command{"wmic", "logicaldisk", "get", "size,freespace,caption"}
	Top                 = NopCommand
	TopH                = NopCommand
	VMState             = NopCommand
	DMesg               = NopCommand
	GC                  = NopCommand
	AppendJavaCoreFiles = Command{"cmd.exe", "/c", "type javacore.* > threaddump.out"}

	shell = NopCommand
)
