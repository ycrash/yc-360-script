package executils

import (
	"reflect"
	"testing"
)

func TestNilCmdHolder(t *testing.T) {
	cmdHolder := Cmd{}
	defer func() {
		if err := recover(); err != nil {
			t.Fatal(err)
		}
	}()
	cmdHolder.Wait()
}

func TestCaptureCmd(t *testing.T) {
	_, err := RunCaptureCmd(123, "echo $pid")
	if err != nil {
		t.Fatal(err)
	}
}

func TestExpandDynamicArgs(t *testing.T) {
	tests := []struct {
		name    string
		cmd     Command
		args    []string
		want    Command
		wantErr bool
	}{
		{
			name: "substitutes a standalone placeholder",
			cmd:  Command{"ps", "-f", "-p", DynamicArg},
			args: []string{"1234"},
			want: Command{"ps", "-f", "-p", "1234"},
		},
		{
			// AddDynamicArg ignores an embedded placeholder; this substitutes
			// it, which is what lets a command build a script for an interpreter.
			name: "substitutes a placeholder inside a larger argument",
			cmd:  Command{"PowerShell.exe", "-Command", `-Filter "ProcessId=` + DynamicArg + `"`},
			args: []string{"1234"},
			want: Command{"PowerShell.exe", "-Command", `-Filter "ProcessId=1234"`},
		},
		{
			name: "substitutes several placeholders in order",
			cmd:  Command{"vmstat", DynamicArg, DynamicArg},
			args: []string{"5", "10"},
			want: Command{"vmstat", "5", "10"},
		},
		{
			// The command keeps its own argv slots, so no shell sees the pipe.
			name: "leaves shell metacharacters untouched",
			cmd:  Command{"pwsh", "-Command", "Get-Thing " + DynamicArg + " | Select-Object -ExpandProperty Name"},
			args: []string{"7"},
			want: Command{"pwsh", "-Command", "Get-Thing 7 | Select-Object -ExpandProperty Name"},
		},
		{
			name:    "reports too few args",
			cmd:     Command{"ps", "-p", DynamicArg},
			args:    nil,
			wantErr: true,
		},
		{
			name:    "reports too many args",
			cmd:     Command{"ps", "-p", DynamicArg},
			args:    []string{"1", "2"},
			wantErr: true,
		},
		{
			name: "returns NopCommand for an empty command",
			cmd:  NopCommand,
			args: []string{"1234"},
			want: NopCommand,
		},
		{
			// Rescanning substituted text would run off the end of args.
			name: "does not rescan a substituted value",
			cmd:  Command{"ps", "-p", DynamicArg},
			args: []string{"a" + DynamicArg + "b"},
			want: Command{"ps", "-p", "a" + DynamicArg + "b"},
		},
		{
			name: "does not rescan a substituted value inside a larger argument",
			cmd:  Command{"pwsh", "-Command", "Get-Thing " + DynamicArg + " " + DynamicArg},
			args: []string{DynamicArg, "tail"},
			want: Command{"pwsh", "-Command", "Get-Thing " + DynamicArg + " tail"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.cmd.ExpandDynamicArgs(tt.args...)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got command %#v", []string(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %#v", []string(got), []string(tt.want))
			}
		})
	}
}

// TestExpandDynamicArgsDoesNotMutateSource guards the shared package-level
// Command vars, which are expanded once per captured process.
func TestExpandDynamicArgsDoesNotMutateSource(t *testing.T) {
	cmd := Command{"ps", "-p", DynamicArg}

	if _, err := cmd.ExpandDynamicArgs("1234"); err != nil {
		t.Fatal(err)
	}
	if cmd[2] != DynamicArg {
		t.Fatalf("source command was mutated: %#v", []string(cmd))
	}
}
