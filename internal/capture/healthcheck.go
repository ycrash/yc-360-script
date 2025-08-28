package capture

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"regexp"
	"strings"
	"time"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

const DefaultTimeoutSeconds = 10

// HealthCheck represents a health check operation for an application endpoint
type HealthCheck struct {
	Capture

	AppName string
	Cfg     config.HealthCheck
}

// Run executes the health check operation against the configured endpoint
// and writes the results to a file. It returns a Result containing the operation
// status and any relevant messages.
func (h *HealthCheck) Run() (Result, error) {
	logger.Log("Running Healthcheck")
	logger.Log("AppName: %s", h.AppName)
	logger.Log("Endpoint: %s", h.Cfg.Endpoint)
	logger.Log("HTTP Body: %s", h.Cfg.HTTPBody)
	logger.Log("Timeout: %d secs", h.Cfg.TimeoutSecs)

	// Create output file
	appName := sanitizeAppNameForFileName(h.AppName)
	fileName := fmt.Sprintf("healthCheckEndpoint.%s.out", appName)
	outFile, err := os.Create(fileName)
	if err != nil {
		return Result{}, fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	// Perform health check and write results
	if err := h.executeAndRecordHealthCheck(outFile); err != nil {
		logToFileAndLogger(outFile, "Health check failed: %v", err)
	}

	// Upload results
	dt := fmt.Sprintf("healthCheckEndpoint&fileName=%s&appName=%s", fileName, appName)
	msg, ok := PostData(h.Endpoint(), dt, outFile)

	return Result{Msg: msg, Ok: ok}, nil
}

// executeAndRecordHealthCheck handles the health check execution and writing results to the file.
// It returns an error if any step fails.
func (h *HealthCheck) executeAndRecordHealthCheck(outFile *os.File) error {
	// Validate endpoint
	if err := h.validateEndpoint(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), h.getTimeoutDuration())
	defer cancel()

	// Execute HTTP health check
	resp, rtt, err := h.runHTTPHealthCheck(ctx)
	if err != nil {
		return fmt.Errorf("HTTP health check failed: %w", err)
	}

	if resp == nil {
		return fmt.Errorf("no response received from health check")
	}

	defer resp.Body.Close()

	// Log response time
	logToFileAndLogger(outFile, "Round Trip Time: %v", rtt)

	// Dump response to file
	err = h.dumpResponse(resp, outFile)
	return err
}

// validateEndpoint checks if the endpoint configuration is valid.
func (h *HealthCheck) validateEndpoint() error {
	if h.Cfg.Endpoint == "" {
		return fmt.Errorf("healthcheck endpoint cannot be empty, please check your configuration")
	}
	if !strings.HasPrefix(h.Cfg.Endpoint, "http") {
		return fmt.Errorf("healthcheck endpoint must start with http:// or https://")
	}
	return nil
}

// dumpResponse writes the HTTP response to the output file.
func (h *HealthCheck) dumpResponse(resp *http.Response, outFile *os.File) error {
	respDump, err := httputil.DumpResponse(resp, true)
	if err != nil {
		return fmt.Errorf("failed to dump response: %w", err)
	}

	if _, err := outFile.Write(respDump); err != nil {
		return fmt.Errorf("failed to write response dump: %w", err)
	}

	return nil
}

// sanitizeAppNameForFileName sanitizes the application name to create a safe filename
// by removing potentially dangerous characters and ensuring the result contains only
// alphanumeric characters, dashes, and underscores.
func sanitizeAppNameForFileName(name string) string {
	// Replace directory traversal characters and path separators
	name = strings.ReplaceAll(name, "..", "")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")

	// Replace other potentially problematic characters
	// Only allow alphanumeric, dash, underscore
	invalidChars := regexp.MustCompile(`[^a-zA-Z0-9\-_]`)
	name = invalidChars.ReplaceAllString(name, "_")

	// Ensure we have at least one valid character
	if len(name) == 0 {
		return "default"
	}

	return name
}

// logToFileAndLogger writes the formatted message to both the logger and the specified file
// This ensures consistent logging across multiple outputs.
func logToFileAndLogger(file *os.File, format string, values ...interface{}) {
	logger.Log(format, values...)
	file.WriteString(fmt.Sprintf(format+"\n", values...))
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:       10,
			IdleConnTimeout:    90 * time.Second,
			DisableCompression: true,
			DisableKeepAlives:  true,
			ForceAttemptHTTP2:  true,
		},
		Timeout: config.GlobalConfig.HttpClientTimeout.Duration(),
	}
}

// runHTTPHealthCheck performs the actual HTTP health check request and measures
// the round-trip time. It returns the response, duration, and any errors encountered.
func (h *HealthCheck) runHTTPHealthCheck(ctx context.Context) (*http.Response, time.Duration, error) {
	httpClient := newHTTPClient()

	req, err := h.buildHealthCheckHTTPRequest(ctx)
	if err != nil {
		return nil, 0, err
	}

	startTime := time.Now()
	resp, err := httpClient.Do(req)
	rtt := time.Since(startTime)

	// Check specifically for timeout errors
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, rtt, fmt.Errorf("Timeout occurred while waiting for a response from %s. The endpoint did not respond within %d seconds",
				h.Cfg.Endpoint,
				h.Cfg.TimeoutSecs)
		}
	}

	return resp, rtt, err
}

// getTimeoutDuration returns the configured timeout duration,
// falling back to the default if the configured value is invalid.
func (h *HealthCheck) getTimeoutDuration() time.Duration {
	timeoutSecs := DefaultTimeoutSeconds
	if h.Cfg.TimeoutSecs < 0 {
		logger.Log("Warning: Negative timeout value provided, using default")
	} else if h.Cfg.TimeoutSecs > 0 {
		timeoutSecs = h.Cfg.TimeoutSecs
	}
	return time.Duration(timeoutSecs) * time.Second
}

// buildHealthCheckHTTPRequest creates an HTTP request for the health check
// with appropriate method (GET/POST), body, and headers based on configuration.
func (h *HealthCheck) buildHealthCheckHTTPRequest(ctx context.Context) (*http.Request, error) {
	reqMethod := "GET"
	var reqBody io.Reader
	if len(h.Cfg.HTTPBody) > 0 {
		reqMethod = "POST"
		reqBody = strings.NewReader(h.Cfg.HTTPBody)
	}

	req, err := http.NewRequestWithContext(ctx, reqMethod, h.Cfg.Endpoint, reqBody)
	if req != nil {
		req.Header.Set("User-Agent", executils.SCRIPT_VERSION)
	}

	return req, err
}
