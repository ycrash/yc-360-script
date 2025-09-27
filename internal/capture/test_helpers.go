package capture

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
)

// skipIfNoJava skips the test if Java is not available in the environment.
// This prevents tests from failing when Java/JDK tools are not installed.
func skipIfNoJava(t *testing.T) {
	if os.Getenv("JAVA_HOME") == "" {
		t.Skip("JAVA_HOME not set, skipping Java-dependent test")
	}

	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java not found in PATH, skipping test")
	}
}

// setupMockServer creates a mock HTTP server for testing that returns HTTP 200 OK.
// The server is automatically closed when the test completes.
func setupMockServer(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	t.Cleanup(server.Close)
	return server
}
