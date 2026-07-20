package cli

import (
	"testing"

	"yc-agent/internal/config"
)

// The pure Node.js validation logic and its full, CI-visible regression suite
// live in the cgo-free internal/nodejsvalidation package (this cli package
// imports "C", so its tests are excluded from the CI subset).
func TestValidateNodejsWrapperMapsToCLIError(t *testing.T) {
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })

	config.GlobalConfig.NodejsCaptureMode = "bogus"
	if err := validateNodejs("nodejs"); err != ErrInvalidArgumentCantContinue {
		t.Errorf("validateNodejs(bogus mode) = %v, want ErrInvalidArgumentCantContinue", err)
	}

	if err := validateNodejsSignal("-x", "SIGUSR1"); err != ErrInvalidArgumentCantContinue {
		t.Errorf("validateNodejsSignal(SIGUSR1) = %v, want ErrInvalidArgumentCantContinue", err)
	}

	config.GlobalConfig.NodejsCaptureMode = "hook"
	if err := validateNodejs("nodejs"); err != nil {
		t.Errorf("validateNodejs(hook mode) = %v, want nil", err)
	}
}
