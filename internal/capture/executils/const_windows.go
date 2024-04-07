package executils

import (
	_ "embed"
	"encoding/base64"
)

//go:embed top.ps1
var topScript []byte

var (
	NetState = Command{"netstat", "-an"}
	PS       = Command{"tasklist"}
	PS2      = Command{"tasklist"}
	M3PS     = Command{WaitCommand, "PowerShell.exe", "-Command", "Get-CimInstance -Class Win32_Process | ConvertTo-Json"}

	Disk                = Command{"wmic", "logicaldisk", "get", "size,freespace,caption"}
	Top                 = NopCommand
	Top2                = NopCommand
	TopH                = NopCommand
	TopH2               = NopCommand
	Top4M3              = NopCommand
	VMState             = Command{WaitCommand, "PowerShell.exe", "-Command", "& {typeperf -sc 10 -si 5 '\\System\\Processor Queue Length' '\\PhysicalDisk(_Total)\\Current Disk Queue Length' '\\Process(_Total)\\Page File Bytes' '\\Memory\\Available KBytes' '\\Memory\\Modified Page List Bytes' '\\Memory\\Cache Bytes' '\\Memory\\Pages Input/sec' '\\Memory\\Pages Output/sec' '\\PhysicalDisk(_Total)\\Disk Transfers/sec' '\\PhysicalDisk(_Total)\\Disk Writes/sec' '\\Processor(_Total)\\Interrupts/sec' '\\System\\Context Switches/sec' '\\Processor(_Total)\\% User Time' '\\Processor(_Total)\\% Privileged Time' '\\Processor(_Total)\\% Idle Time' '\\Processor(_Total)\\% Interrupt Time' '\\Processor(_Total)\\% DPC Time'}"}
	DMesg               = Command{WaitCommand, "PowerShell.exe", "-Command", "& {Get-EventLog -LogName System -Newest 20 -EntryType Error,FailureAudit,Warning | Select-Object TimeGenerated, EntryType, Message | ForEach-Object { Write-Host \"$($_.TimeGenerated) [$($_.EntryType)]: $($_.Message)\" }}"}
	DMesg2              = Command{"wevtutil", "qe", "System", "/c:20", "/rd:true", "/f:text"}
	GC                  = Command{"wmic", "process", "where", DynamicArg, "get", "ProcessId,Commandline"}
	AppendJavaCoreFiles = Command{"cmd.exe", "/c", "type javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"cmd.exe", "/c", "type topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{WaitCommand, "PowerShell.exe", "-Command", "& {Get-WmiObject Win32_PerfFormattedData_PerfProc_Process | Select-Object -Property Name, IDProcess, @{Name=\"PercentProcessorTime\"; Expression={(\"{0:P1}\" -f ($_.PercentProcessorTime/100))}}, @{Name=\"WSP(MB)\"; Expression={[int]($_.WorkingSetPrivate/1mb)}} | Where-Object {$_.Name -notmatch \"^(idle|_total|system)$\"} | Sort-Object -Property PercentProcessorTime -Descending | Format-Table @{Label=\"Name\"; Expression={$_.Name}}, @{Label=\"ID\"; Expression={$_.IDProcess}}, User, @{Label=\"CPU\"; Expression={$_.PercentProcessorTime}}, @{Label=\"Memory\"; Expression={$_.\"WSP(MB)\"}} -AutoSize}"}
	ProcessTopMEM       = Command{WaitCommand, "PowerShell.exe", "-Command", "& {Get-WmiObject Win32_PerfFormattedData_PerfProc_Process | Select-Object -Property Name, IDProcess, @{Name=\"PercentProcessorTime\"; Expression={(\"{0:P1}\" -f ($_.PercentProcessorTime/100))}}, @{Name=\"WSP(MB)\"; Expression={[int]($_.WorkingSetPrivate/1mb)}} | Where-Object {$_.Name -notmatch \"^(idle|_total|system)$\"} | Sort-Object -Property \"WSP(MB)\" -Descending | Format-Table @{Label=\"Name\"; Expression={$_.Name}}, @{Label=\"ID\"; Expression={$_.IDProcess}}, User, @{Label=\"CPU\"; Expression={$_.PercentProcessorTime}}, @{Label=\"Memory\"; Expression={$_.\"WSP(MB)\"}} -AutoSize}"}
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
