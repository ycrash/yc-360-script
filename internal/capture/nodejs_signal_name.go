package capture

import (
	"fmt"
	"strings"
)

var nodeSupportedSignalNames = map[string]string{
	"SIGUSR2": "SIGUSR2",
	"USR2":    "SIGUSR2",
	"SIGQUIT": "SIGQUIT",
	"QUIT":    "SIGQUIT",
	"SIGABRT": "SIGABRT",
	"ABRT":    "SIGABRT",
}

// NormalizeNodeSignalName validates and canonicalizes a Node signal-mode signal
// name. SIGUSR1 is refused because Node always uses it to activate the inspector.
func NormalizeNodeSignalName(name string) (string, error) {
	n := strings.ToUpper(strings.TrimSpace(name))
	if n == "" {
		return "", fmt.Errorf("signal must be set (e.g. SIGUSR2)")
	}
	if n == "SIGUSR1" || n == "USR1" {
		return "", fmt.Errorf("SIGUSR1 is refused: it unconditionally activates Node's inspector (security exposure)")
	}
	canonical, ok := nodeSupportedSignalNames[n]
	if !ok {
		return "", fmt.Errorf("unsupported signal %q; use one of: SIGUSR2, SIGQUIT, SIGABRT", name)
	}
	return canonical, nil
}

// ValidateNodeSignalName reports whether name is usable for Node signal mode.
func ValidateNodeSignalName(name string) error {
	_, err := NormalizeNodeSignalName(name)
	return err
}
