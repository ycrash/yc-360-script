package shell

var (
	NetState            = Command{"netstat", "-an"}
	PS                  = Command{"tasklist"}
	M3PS                = Command{"wmic", "process", "where", DynamicArg, "get", "Name,ProcessId"}
	Disk                = Command{"wmic", "logicaldisk", "get", "size,freespace,caption"}
	Top                 = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc cpu | select -first 30}"}
	TopH                = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc cpu | select -first 30}"}
	VMState             = NopCommand
	DMesg               = NopCommand
	GC                  = NopCommand
	AppendJavaCoreFiles = Command{"cmd.exe", "/c", "type javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"cmd.exe", "/c", "type topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc CPU}"}
	ProcessTopMEM       = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc PM}"}

	SHELL = Command{"cmd.exe", "/c"}
)
