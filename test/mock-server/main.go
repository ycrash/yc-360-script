package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// UploadMetadata stores metadata about uploaded files
type UploadMetadata struct {
	DataType   string    `json:"dataType"`
	Size       int64     `json:"size"`
	UploadTime time.Time `json:"uploadTime"`
	IsValid    bool      `json:"isValid"`
	Error      string    `json:"error,omitempty"`
}

type MockServer struct {
	mu             sync.RWMutex
	uploads        map[string]map[string]bool           // app -> dataType -> received
	uploadMetadata map[string]map[string]UploadMetadata // app -> dataType -> metadata
	m3Uploads      map[string]map[string]int            // app -> dataType -> count
}

func NewMockServer() *MockServer {
	return &MockServer{
		uploads:        make(map[string]map[string]bool),
		uploadMetadata: make(map[string]map[string]UploadMetadata),
		m3Uploads:      make(map[string]map[string]int),
	}
}

// validateDataType checks if the data type is valid
func validateDataType(dt string) bool {
	validTypes := map[string]bool{
		"gc": true, "td": true, "hd": true, "top": true,
		"vmstat": true, "ps": true, "ns": true, "dmesg": true,
		"ping": true, "disk": true, "appLog": true, "accessLog": true,
		"lp": true, "hdsub": true, "ed": true,
	}
	return validTypes[dt]
}

func (s *MockServer) HandleReceiver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Enhanced Request Validation - Validate API Key
	apiKey := r.Header.Get("ApiKey")
	if apiKey == "" {
		http.Error(w, "Missing ApiKey header", http.StatusUnauthorized)
		return
	}

	// Enhanced Request Validation - Validate required parameters
	app := r.URL.Query().Get("an")
	dt := r.URL.Query().Get("dt")

	if app == "" {
		http.Error(w, "Missing 'an' (application name) parameter", http.StatusBadRequest)
		return
	}

	if dt == "" {
		http.Error(w, "Missing 'dt' (data type) parameter", http.StatusBadRequest)
		return
	}

	// Enhanced Request Validation - Validate data type
	if !validateDataType(dt) {
		http.Error(w, fmt.Sprintf("Invalid data type: %s", dt), http.StatusBadRequest)
		return
	}

	// Parse multipart form
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	log.Printf("Received upload: app=%s, dt=%s, apiKey=%s", app, dt, apiKey)

	// File Content Validation - Read and validate file
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Missing file in multipart form", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read file to buffer to check size/content
	var buf bytes.Buffer
	size, err := io.Copy(&buf, file)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read file: %v", err), http.StatusInternalServerError)
		return
	}

	metadata := UploadMetadata{
		DataType:   dt,
		Size:       size,
		UploadTime: time.Now(),
		IsValid:    size > 0,
	}

	if size == 0 {
		metadata.Error = "Empty file"
		metadata.IsValid = false
	}

	log.Printf("Received file: %s (size: %d bytes, valid: %v)", header.Filename, size, metadata.IsValid)

	// Track upload
	s.mu.Lock()
	if s.uploads[app] == nil {
		s.uploads[app] = make(map[string]bool)
	}
	s.uploads[app][dt] = true

	// Store metadata
	if s.uploadMetadata[app] == nil {
		s.uploadMetadata[app] = make(map[string]UploadMetadata)
	}
	s.uploadMetadata[app][dt] = metadata
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("success"))
}

func (s *MockServer) HandleFin(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	response := map[string]string{
		"status": "success",
		"url":    "http://mock-server:8080/analysis/test-123",
	}
	json.NewEncoder(w).Encode(response)
}

// HandleM3Receiver handles M3 mode periodic metric uploads
func (s *MockServer) HandleM3Receiver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Enhanced Request Validation - Validate API Key
	apiKey := r.Header.Get("ApiKey")
	if apiKey == "" {
		http.Error(w, "Missing ApiKey header", http.StatusUnauthorized)
		return
	}

	// Enhanced Request Validation - Validate required parameters
	app := r.URL.Query().Get("an")
	dt := r.URL.Query().Get("dt")

	if app == "" {
		http.Error(w, "Missing 'an' (application name) parameter", http.StatusBadRequest)
		return
	}

	if dt == "" {
		http.Error(w, "Missing 'dt' (data type) parameter", http.StatusBadRequest)
		return
	}

	// Enhanced Request Validation - Validate data type
	if !validateDataType(dt) {
		http.Error(w, fmt.Sprintf("Invalid data type: %s", dt), http.StatusBadRequest)
		return
	}

	// Parse multipart form
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	timestamp := r.URL.Query().Get("ts")
	timezone := r.URL.Query().Get("tz")

	log.Printf("M3 upload: app=%s, dt=%s, ts=%s, tz=%s, apiKey=%s", app, dt, timestamp, timezone, apiKey)

	// File Content Validation - Read and validate file
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Missing file in multipart form", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read file to buffer to check size/content
	var buf bytes.Buffer
	size, err := io.Copy(&buf, file)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to read file: %v", err), http.StatusInternalServerError)
		return
	}

	metadata := UploadMetadata{
		DataType:   dt,
		Size:       size,
		UploadTime: time.Now(),
		IsValid:    size > 0,
	}

	if size == 0 {
		metadata.Error = "Empty file"
		metadata.IsValid = false
	}

	log.Printf("Received M3 file: %s (size: %d bytes, valid: %v)", header.Filename, size, metadata.IsValid)

	// Track M3 uploads separately
	s.mu.Lock()
	if s.m3Uploads[app] == nil {
		s.m3Uploads[app] = make(map[string]int)
	}
	s.m3Uploads[app][dt]++

	// Store metadata
	if s.uploadMetadata[app] == nil {
		s.uploadMetadata[app] = make(map[string]UploadMetadata)
	}
	s.uploadMetadata[app][dt] = metadata
	s.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("success"))
}

// HandleM3Fin handles M3 mode completion acknowledgment
func (s *MockServer) HandleM3Fin(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	response := map[string]string{
		"status": "success",
		"url":    "http://mock-server:8080/m3-analysis/test-123",
	}
	json.NewEncoder(w).Encode(response)
}

func (s *MockServer) HandleVerify(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")

	s.mu.RLock()
	uploads := s.uploads[app]
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(uploads)
}

// HandleMetadata returns upload metadata for verification
func (s *MockServer) HandleMetadata(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")

	s.mu.RLock()
	metadata := s.uploadMetadata[app]
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(metadata)
}

// HandleM3Verify returns M3 upload counts for verification
func (s *MockServer) HandleM3Verify(w http.ResponseWriter, r *http.Request) {
	app := r.URL.Query().Get("app")

	s.mu.RLock()
	uploads := s.m3Uploads[app]
	s.mu.RUnlock()

	json.NewEncoder(w).Encode(uploads)
}

func (s *MockServer) HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func main() {
	server := NewMockServer()

	// OnDemand mode endpoints
	http.HandleFunc("/ycrash-receiver", server.HandleReceiver)
	http.HandleFunc("/yc-fin", server.HandleFin)

	// M3 mode endpoints
	http.HandleFunc("/m3-receiver", server.HandleM3Receiver)
	http.HandleFunc("/m3-fin", server.HandleM3Fin)

	// Verification and health endpoints
	http.HandleFunc("/verify-uploads", server.HandleVerify)
	http.HandleFunc("/verify-metadata", server.HandleMetadata)
	http.HandleFunc("/verify-m3", server.HandleM3Verify)
	http.HandleFunc("/health", server.HandleHealth)

	fmt.Println("Mock yCrash server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
