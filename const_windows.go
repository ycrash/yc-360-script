package shell

import (
	_ "embed"
	"encoding/base64"
)

//go:embed top.ps1
var topScript []byte

var (
	NetState            = Command{"netstat", "-an"}
	PS                  = Command{"tasklist"}
	PS2                 = Command{"tasklist"}
	M3PS                = Command{"wmic", "process", "where", DynamicArg, "get", "Name,ProcessId,Commandline"}
	Disk                = Command{"wmic", "logicaldisk", "get", "size,freespace,caption"}
	Top                 = NopCommand
	Top2                = NopCommand
	TopH                = NopCommand
	TopH2               = NopCommand
	Top4M3              = NopCommand
	VMState             = Command{WaitCommand, "PowerShell.exe", "-Command", "& {typeperf -sc 10 -si 5 '\\Process(_Total)\\Page File Bytes' '\\Memory\\Available KBytes' '\\Memory\\Standby Cache Reserve Bytes' '\\Memory\\Pages Input/sec' '\\Memory\\Pages Output/sec' '\\PhysicalDisk(_Total)\\Disk Transfers/sec' '\\PhysicalDisk(_Total)\\Disk Writes/sec' '\\Processor(_Total)\\Interrupts/sec' '\\System\\Context Switches/sec' '\\Processor(_Total)\\%% User Time' '\\Processor(_Total)\\%% Privileged Time' '\\Processor(_Total)\\%% Idle Time'}"}
	DMesg               = Command{WaitCommand, "PowerShell.exe", "-Command", "& {Get-EventLog -LogName System -Newest 20 -EntryType Error,FailureAudit,Warning | Select-Object TimeGenerated, EntryType, Message | ForEach-Object { Write-Host \"$($_.TimeGenerated) [$($_.EntryType)]: $($_.Message)\" }}"}
	DMesg2              = Command{"wevtutil", "qe", "System", "/c:20", "/rd:true", "/f:text"}
	GC                  = Command{"wmic", "process", "where", DynamicArg, "get", "ProcessId,Commandline"}
	AppendJavaCoreFiles = Command{"cmd.exe", "/c", "type javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"cmd.exe", "/c", "type topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc CPU}"}
	ProcessTopMEM       = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc PM}"}
	OSVersion           = Command{WaitCommand, "PowerShell.exe", "-Command", "& {systeminfo | findstr /B /C:\"OS Name\" /C:\"OS Version\"}"}
	KernelParam         = NopCommand
	Ping                = Command{WaitCommand, "ping", "-n", "6"}
	JavaVersionCommand  = Command{"java.exe", "-XshowSettings:java", "-version"}

	SHELL = Command{"cmd.exe", "/c"}
)

func init() {
	encodedScript := base64.StdEncoding.EncodeToString(topScript[2:])
	Top = Command{WaitCommand, "PowerShell.exe", "-encodedCommand", encodedScript}
	TopH = Command{WaitCommand, "PowerShell.exe", "-encodedCommand", encodedScript}
	Top4M3 = Command{WaitCommand, "PowerShell.exe", "-encodedCommand", encodedScript}
}
