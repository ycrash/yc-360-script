package capture

import (
	"io"
	"os"

	"yc-agent/internal/logger"
)

const accessLogOut = "accesslog.out"

// AccessLog represents struct that captures and uploads the specified log file.
//
// Deprecated: use App logs auto discovery instead.
type AccessLog struct {
	Capture
	SourcePath  string // Source path of the access log file
	CapturePath string // Destination path to save the captured data
	Position    int64  // Current read position in the log file
}

// Run captures the new content from the access log specified in the Path field to a file,
// then uploads them to the server.
func (al *AccessLog) Run() (Result, error) {
	if al.SourcePath == "" {
		return Result{}, nil
	}

	capturedFile, err := al.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	result := al.UploadCapturedFile(capturedFile)
	return result, nil
}

// CaptureToFile reads new contents from the source file starting from Position
// and writes them to a new capture file. It updates Position to track the last read
// location for subsequent calls.
//
// The capture file is created at accessLogOut path. The caller is responsible for
// closing the returned file.
func (al *AccessLog) CaptureToFile() (*os.File, error) {
	if al.CapturePath == "" {
		al.CapturePath = accessLogOut
	}

	// Open the access log path as the source
	src, err := os.Open(al.SourcePath)
	if err != nil {
		return nil, err
	}
	defer src.Close()

	// Set offset to the last position
	_, err = src.Seek(al.Position, io.SeekStart)
	if err != nil {
		return nil, err
	}

	// Create the destination file
	dst, err := os.Create(al.CapturePath)
	if err != nil {
		return nil, err
	}

	// Copy src to dst
	copied, err := io.Copy(dst, src)
	if err != nil {
		return nil, err
	}

	// Ensure all data is persisted to disk before proceeding with upload
	if syncErr := dst.Sync(); syncErr != nil {
		logger.Log("failed to sync destination file: %v", syncErr)
	}

	// Update the position field to track the current position
	// So that in the next call, it will start from this position.
	al.Position += copied

	return dst, nil
}

// UploadCapturedFile sends the captured log file to the configured endpoint
// with data type "accessLog".
func (al *AccessLog) UploadCapturedFile(f *os.File) Result {
	msg, ok := PostData(al.Endpoint(), "accessLog", f)
	return Result{Msg: msg, Ok: ok}
}
