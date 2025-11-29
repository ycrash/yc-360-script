package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnhancedRequestValidation(t *testing.T) {
	server := NewMockServer()

	tests := []struct {
		name           string
		method         string
		endpoint       string
		apiKey         string
		params         map[string]string
		includeFile    bool
		expectedStatus int
		expectedError  string
	}{
		{
			name:           "Missing API Key",
			method:         "POST",
			endpoint:       "/ycrash-receiver",
			apiKey:         "",
			params:         map[string]string{"an": "test-app", "dt": "gc"},
			includeFile:    true,
			expectedStatus: http.StatusUnauthorized,
			expectedError:  "Missing ApiKey header",
		},
		{
			name:           "Missing application name",
			method:         "POST",
			endpoint:       "/ycrash-receiver",
			apiKey:         "test-key",
			params:         map[string]string{"dt": "gc"},
			includeFile:    true,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "Missing 'an' (application name) parameter",
		},
		{
			name:           "Missing data type",
			method:         "POST",
			endpoint:       "/ycrash-receiver",
			apiKey:         "test-key",
			params:         map[string]string{"an": "test-app"},
			includeFile:    true,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "Missing 'dt' (data type) parameter",
		},
		{
			name:           "Invalid data type",
			method:         "POST",
			endpoint:       "/ycrash-receiver",
			apiKey:         "test-key",
			params:         map[string]string{"an": "test-app", "dt": "invalid"},
			includeFile:    true,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "Invalid data type: invalid",
		},
		{
			name:           "Missing file",
			method:         "POST",
			endpoint:       "/ycrash-receiver",
			apiKey:         "test-key",
			params:         map[string]string{"an": "test-app", "dt": "gc"},
			includeFile:    false,
			expectedStatus: http.StatusBadRequest,
			expectedError:  "Missing file in multipart form",
		},
		{
			name:           "Valid request",
			method:         "POST",
			endpoint:       "/ycrash-receiver",
			apiKey:         "test-key",
			params:         map[string]string{"an": "test-app", "dt": "gc"},
			includeFile:    true,
			expectedStatus: http.StatusOK,
		},
		{
			name:           "Wrong HTTP method",
			method:         "GET",
			endpoint:       "/ycrash-receiver",
			apiKey:         "test-key",
			params:         map[string]string{"an": "test-app", "dt": "gc"},
			includeFile:    false,
			expectedStatus: http.StatusMethodNotAllowed,
			expectedError:  "Method not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)

			if tt.includeFile {
				part, err := writer.CreateFormFile("file", "test.log")
				if err != nil {
					t.Fatal(err)
				}
				part.Write([]byte("test content"))
			}
			writer.Close()

			url := tt.endpoint + "?"
			for k, v := range tt.params {
				url += k + "=" + v + "&"
			}

			req := httptest.NewRequest(tt.method, url, &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			if tt.apiKey != "" {
				req.Header.Set("ApiKey", tt.apiKey)
			}

			w := httptest.NewRecorder()

			switch tt.endpoint {
			case "/ycrash-receiver":
				server.HandleReceiver(w, req)
			case "/m3-receiver":
				server.HandleM3Receiver(w, req)
			}

			if w.Code != tt.expectedStatus {
				t.Errorf("Expected status %d, got %d. Body: %s", tt.expectedStatus, w.Code, w.Body.String())
			}

			if tt.expectedError != "" {
				responseBody := w.Body.String()
				if !contains(responseBody, tt.expectedError) {
					t.Errorf("Expected error message to contain '%s', got '%s'", tt.expectedError, responseBody)
				}
			}
		})
	}
}

func TestFileContentValidation(t *testing.T) {
	server := NewMockServer()

	tests := []struct {
		name        string
		fileContent string
		expectValid bool
	}{
		{
			name:        "Non-empty file",
			fileContent: "This is test content",
			expectValid: true,
		},
		{
			name:        "Empty file",
			fileContent: "",
			expectValid: false,
		},
		{
			name:        "Large file",
			fileContent: string(make([]byte, 1024*1024)), // 1MB
			expectValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)

			part, err := writer.CreateFormFile("file", "test.log")
			if err != nil {
				t.Fatal(err)
			}
			part.Write([]byte(tt.fileContent))
			writer.Close()

			req := httptest.NewRequest("POST", "/ycrash-receiver?an=test-app&dt=gc", &body)
			req.Header.Set("Content-Type", writer.FormDataContentType())
			req.Header.Set("ApiKey", "test-key")

			w := httptest.NewRecorder()
			server.HandleReceiver(w, req)

			if w.Code != http.StatusOK {
				t.Errorf("Expected status 200, got %d", w.Code)
			}

			// Check metadata
			server.mu.RLock()
			metadata := server.uploadMetadata["test-app"]["gc"]
			server.mu.RUnlock()

			if metadata.IsValid != tt.expectValid {
				t.Errorf("Expected IsValid=%v, got %v", tt.expectValid, metadata.IsValid)
			}

			if metadata.Size != int64(len(tt.fileContent)) {
				t.Errorf("Expected Size=%d, got %d", len(tt.fileContent), metadata.Size)
			}

			if !tt.expectValid && metadata.Error != "Empty file" {
				t.Errorf("Expected error 'Empty file', got '%s'", metadata.Error)
			}
		})
	}
}

func TestM3ModeEndpoints(t *testing.T) {
	server := NewMockServer()

	// Test M3 Receiver
	t.Run("M3 Receiver", func(t *testing.T) {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)

		part, err := writer.CreateFormFile("file", "m3-metrics.log")
		if err != nil {
			t.Fatal(err)
		}
		part.Write([]byte("m3 metrics data"))
		writer.Close()

		req := httptest.NewRequest("POST", "/m3-receiver?an=m3-app&dt=top&ts=2024-01-01&tz=UTC", &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.Header.Set("ApiKey", "m3-test-key")

		w := httptest.NewRecorder()
		server.HandleM3Receiver(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d. Body: %s", w.Code, w.Body.String())
		}

		// Verify M3 upload was tracked
		server.mu.RLock()
		count := server.m3Uploads["m3-app"]["top"]
		server.mu.RUnlock()

		if count != 1 {
			t.Errorf("Expected 1 M3 upload, got %d", count)
		}
	})

	// Test M3 Fin
	t.Run("M3 Fin", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/m3-fin", nil)
		w := httptest.NewRecorder()
		server.HandleM3Fin(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", w.Code)
		}

		var response map[string]string
		json.NewDecoder(w.Body).Decode(&response)

		if response["status"] != "success" {
			t.Errorf("Expected status 'success', got '%s'", response["status"])
		}

		if response["url"] == "" {
			t.Error("Expected URL in response")
		}
	})
}

func TestValidDataTypes(t *testing.T) {
	validTypes := []string{
		"gc", "td", "hd", "top", "vmstat", "ps", "ns",
		"dmesg", "ping", "disk", "appLog", "accessLog",
		"lp", "hdsub", "ed",
	}

	for _, dt := range validTypes {
		if !validateDataType(dt) {
			t.Errorf("Expected data type '%s' to be valid", dt)
		}
	}

	invalidTypes := []string{"invalid", "unknown", "test", ""}
	for _, dt := range invalidTypes {
		if validateDataType(dt) {
			t.Errorf("Expected data type '%s' to be invalid", dt)
		}
	}
}

func TestVerificationEndpoints(t *testing.T) {
	server := NewMockServer()

	// Add some test data
	server.mu.Lock()
	server.uploads["app1"] = map[string]bool{"gc": true, "td": true}
	server.uploadMetadata["app1"] = map[string]UploadMetadata{
		"gc": {DataType: "gc", Size: 1024, IsValid: true},
	}
	server.m3Uploads["m3app"] = map[string]int{"top": 5}
	server.mu.Unlock()

	t.Run("Verify Uploads", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/verify-uploads?app=app1", nil)
		w := httptest.NewRecorder()
		server.HandleVerify(w, req)

		var uploads map[string]bool
		json.NewDecoder(w.Body).Decode(&uploads)

		if !uploads["gc"] || !uploads["td"] {
			t.Error("Expected gc and td uploads")
		}
	})

	t.Run("Verify Metadata", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/verify-metadata?app=app1", nil)
		w := httptest.NewRecorder()
		server.HandleMetadata(w, req)

		var metadata map[string]UploadMetadata
		json.NewDecoder(w.Body).Decode(&metadata)

		if metadata["gc"].Size != 1024 {
			t.Errorf("Expected size 1024, got %d", metadata["gc"].Size)
		}
	})

	t.Run("Verify M3", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/verify-m3?app=m3app", nil)
		w := httptest.NewRecorder()
		server.HandleM3Verify(w, req)

		var uploads map[string]int
		json.NewDecoder(w.Body).Decode(&uploads)

		if uploads["top"] != 5 {
			t.Errorf("Expected 5 M3 uploads, got %d", uploads["top"])
		}
	})
}

func TestHealthEndpoint(t *testing.T) {
	server := NewMockServer()

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	server.HandleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	body, _ := io.ReadAll(w.Body)
	if string(body) != "ok" {
		t.Errorf("Expected body 'ok', got '%s'", string(body))
	}
}

// Helper function
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && bytesContains([]byte(s), []byte(substr))))
}

func bytesContains(b, subslice []byte) bool {
	for i := 0; i <= len(b)-len(subslice); i++ {
		if string(b[i:i+len(subslice)]) == string(subslice) {
			return true
		}
	}
	return false
}
