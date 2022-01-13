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
	VMState             = NopCommand
	DMesg               = NopCommand
	DMesg2              = NopCommand
	GC                  = NopCommand
	AppendJavaCoreFiles = Command{"cmd.exe", "/c", "type javacore.* > threaddump.out"}
	AppendTopHFiles     = Command{"cmd.exe", "/c", "type topdashH.* >> threaddump.out"}
	ProcessTopCPU       = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc CPU}"}
	ProcessTopMEM       = Command{WaitCommand, "PowerShell.exe", "-Command", "& {ps | sort -desc PM}"}
	OSVersion           = Command{WaitCommand, "PowerShell.exe", "-Command", "& {systeminfo | findstr /B /C:\"OS Name\" /C:\"OS Version\"}"}
	KernelParam         = NopCommand
	Ping                = Command{WaitCommand, "ping", "-n", "6"}

	SHELL = Command{"cmd.exe", "/c"}
)

func init() {
	encodedScript := base64.StdEncoding.EncodeToString(topScript[2:])
	Top = Command{WaitCommand, "PowerShell.exe", "-encodedCommand", encodedScript}
	TopH = Command{WaitCommand, "PowerShell.exe", "-encodedCommand", encodedScript}
	Top4M3 = Command{WaitCommand, "PowerShell.exe", "-encodedCommand", encodedScript}
}
