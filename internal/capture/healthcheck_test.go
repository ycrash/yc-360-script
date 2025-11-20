package capture

import (
	"testing"
	"yc-agent/internal/config"
)

// test case will assert whether the result is OK that is received
// as part of the yc server upload
func TestHealthCheck_Run(t *testing.T) {
	// TODO: Revisit this test - currently failing in CI
	// Test attempts to make HTTP requests to localhost:8080 which is not running in CI.
	// Needs proper HTTP server mocking or test server setup.
	t.Skip("Skipping until HTTP server can be properly mocked")

	appName := "TestApp"
	endpoint := "http://localhost:8080/"

	h := &HealthCheck{
		AppName: appName,
		Cfg: config.HealthCheck{
			Endpoint:    endpoint,
			HTTPBody:    `{"status":"ok"}`,
			TimeoutSecs: 2,
		},
	}
	h.SetEndpoint("http://localhost:8080/?dt=healthCheckEndpoint&fileName=healthCheckEndpoint.TestApp.out&appName=TestApp")

	result, err := h.Run()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Ok {
		t.Fatalf("expected result.Ok=true, got false. Msg: %s", result.Msg)
	}
}
