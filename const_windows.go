package shell

var (
	NetState            = Command{"netstat", "-an"}
	PS                  = Command{"tasklist"}
	Disk                = Command{"wmic", "logicaldisk", "get", "size,freespace,caption"}
	Top                 = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc cpu | select -first 30}"}
	TopH                = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc cpu | select -first 30}"}
	VMState             = NopCommand
	DMesg               = NopCommand
	GC                  = NopCommand
	AppendJavaCoreFiles = Command{"cmd.exe", "/c", "type javacore.* > threaddump.out"}

	SHELL = Command{"cmd.exe", "/c"}
)
