package capture

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

// Taken from yc-server
var compressedHeapExtensions = []string{
	"zip",
	"gz",
	"pigz",
	"7z",
	"xz",
	"z",
	"lzma",
	"deflate",
	"sz",
	"lz4",
	"zstd",
	"bz2",
	"tgz",
	"tar",
	"tar.gz",
}

const hdOut = "heap_dump.out"
const hdZip = "heap_dump.zip"

type HeapDump struct {
	Capture
	JavaHome string
	Pid      int
	hdPath   string
	dump     bool
}

// NewHeapDump creates a new HeapDump instance with the provided parameters.
func NewHeapDump(javaHome string, pid int, hdPath string, dump bool) *HeapDump {
	return &HeapDump{
		JavaHome: javaHome,
		Pid:      pid,
		hdPath:   hdPath,
		dump:     dump,
	}
}

// getDestHeapDumpFilename returns the appropriate filename based on compression status
func (t *HeapDump) getDestHeapDumpFilename(isCompressed bool) string {
	if isCompressed {
		return hdZip
	}
	return hdOut
}

// Run executes the heap dump capture process and uploads the captured file
// to the specified endpoint.
func (t *HeapDump) Run() (Result, error) {
	var hd *os.File
	var err error
	var isCompressed bool
	var contentEncoding string

	// Try pre-captured heap dump first
	if len(t.hdPath) > 0 {
		isCompressed, contentEncoding = isCompressedHeapFile(t.hdPath)
		if isCompressed {
			logger.Log("detected pre-compressed heap dump file: %s with encoding: %s", t.hdPath, contentEncoding)
		}

		hd, err = t.getPreCapturedDumpFile(isCompressed)
		if err != nil {
			return Result{
				Msg: fmt.Sprintf("capture heap dump failed: %s", err.Error()),
				Ok:  false,
			}, nil
		}
	} else if t.Pid > 0 && t.dump {
		var actualDumpPath string
		// Then try capturing a new heap dump
		hd, actualDumpPath, err = t.captureDumpFile()
		if err != nil {
			return Result{
				Msg: fmt.Sprintf("capture heap dump failed: %s", err.Error()),
				Ok:  false,
			}, nil
		}

		// Cleanup the actual dump file after we're done with it
		if actualDumpPath != "" {
			defer func() {
				err := os.Remove(actualDumpPath)
				if err != nil {
					logger.Trace().Err(err).Str("file", actualDumpPath).Msg("failed to rm hd file")
				}
			}()
		}
	}

	if hd == nil {
		return Result{Msg: "skipped heap dump"}, nil
	}

	tempFilename := t.getDestHeapDumpFilename(isCompressed)

	defer func() {
		err := hd.Close()
		if err != nil && !errors.Is(err, os.ErrClosed) {
			logger.Log("failed to close hd file %s cause err: %s", tempFilename, err.Error())
		}
	}()

	var fileToUpload *os.File
	var uploadContentEncoding string

	if isCompressed {
		// If the file is already compressed, use it directly without re-compressing
		logger.Log("file is already compressed, skipping compression step")
		fileToUpload = hd
		uploadContentEncoding = contentEncoding
	} else {
		// For uncompressed files, compress them
		logger.Log("captured heap dump data, zipping...")

		zipfile, err := t.CreateZipFile(hd)
		if err != nil {
			return Result{
				Msg: fmt.Sprintf("capture heap dump failed: %s", err.Error()),
				Ok:  false,
			}, nil
		}

		defer func() {
			if err := zipfile.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				logger.Debug().Err(err).Msg("failed to close zip file")
			}
		}()

		defer func() {
			err = os.Remove(tempFilename)
			if err != nil {
				logger.Log("failed to rm hd file %s cause err: %s", tempFilename, err.Error())
			}
		}()

		fileToUpload = zipfile
		uploadContentEncoding = "zip"
	}

	result := t.UploadCapturedFile(fileToUpload, uploadContentEncoding)
	return result, nil
}

// getPreCapturedDumpFile handles the case when a heap dump is pre-captured (using the hdPath field)
func (t *HeapDump) getPreCapturedDumpFile(isCompressed bool) (*os.File, error) {
	hdf, err := os.Open(t.hdPath)

	// Fallback, try to open the file in the Docker container
	if err != nil && runtime.GOOS == "linux" {
		logger.Log("failed to open hdPath(%s) err: %s. Trying to open in the Docker container...", t.hdPath, err.Error())
		hdf, err = os.Open(filepath.Join("/proc", strconv.Itoa(t.Pid), "root", t.hdPath))
	}

	if err != nil {
		logger.Log("failed to open hdPath(%s) err: %s", t.hdPath, err.Error())
		return nil, err
	}

	logger.Log("copying heap dump data %s", t.hdPath)

	defer func() {
		err := hdf.Close()
		if err != nil {
			logger.Log("failed to close hd file %s cause err: %s", t.hdPath, err.Error())
		}
	}()

	filename := t.getDestHeapDumpFilename(isCompressed)
	hd, err := os.Create(filename)
	if err != nil {
		return nil, err
	}

	_, err = io.Copy(hd, hdf)
	if err != nil {
		return nil, err
	}

	_, err = hd.Seek(0, 0)
	if err != nil {
		return nil, err
	}

	logger.Log("copied heap dump data %s to %s", t.hdPath, filename)
	return hd, nil
}

// captureDumpFile handles the case when a heap dump needs to be captured (using the Pid field)
// and returns both the file handle and the actual dump path
func (t *HeapDump) captureDumpFile() (*os.File, string, error) {
	logger.Log("capturing heap dump data")

	dir, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}

	fp := filepath.Join(dir, fmt.Sprintf("%s.%d.%d", hdOut, t.Pid, time.Now().Unix()))
	actualDumpPath, err := t.heapDump(fp)
	if err != nil {
		// Fallback if the heap dump failed
		// Retry with a temp file, hopefully writeable
		fp = filepath.Join(os.TempDir(), fmt.Sprintf("%s.%d.%d", hdOut, t.Pid, time.Now().Unix()))
		actualDumpPath, err = t.heapDump(fp)

		if err != nil {
			return nil, "", err
		}
	}

	hd, err := os.Open(actualDumpPath)
	if err != nil && runtime.GOOS == "linux" {
		// Fallback, try to open the file in the Docker container
		logger.Log("Failed to %s. Trying to open in the Docker container...", err.Error())
		actualDumpPath = filepath.Join("/proc", strconv.Itoa(t.Pid), "root", actualDumpPath)
		hd, err = os.Open(actualDumpPath)
	}

	if err != nil {
		return nil, actualDumpPath, fmt.Errorf("failed to open heap dump file: %w", err)
	}

	return hd, actualDumpPath, nil
}

func (t *HeapDump) CreateZipFile(hd *os.File) (*os.File, error) {
	zipfile, err := os.Create(hdZip)
	if err != nil {
		return nil, fmt.Errorf("failed to create zip file: %w", err)
	}

	writer := zip.NewWriter(bufio.NewWriter(zipfile))
	out, err := writer.Create(hdOut)
	if err != nil {
		return nil, fmt.Errorf("failed to create zip file: %w", err)
	}

	_, err = io.Copy(out, hd)
	if err != nil {
		return nil, fmt.Errorf("failed to zip heap dump file: %w", err)
	}

	err = writer.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to finish zipping heap dump file: %w", err)
	}

	e := zipfile.Sync()
	if e != nil && !errors.Is(e, os.ErrClosed) {
		logger.Log("failed to sync file %s", e)
	}

	return zipfile, nil
}

func (t *HeapDump) UploadCapturedFile(file *os.File, contentEncoding string) Result {
	msg, ok := PostData(t.Endpoint(), fmt.Sprintf("hd&Content-Encoding=%s", contentEncoding), file)

	return Result{
		Msg: msg,
		Ok:  ok,
	}
}

// isCompressedHeapFile checks if the given file is a compressed heap dump file
// and returns the extension as the MIME type if it is.
func isCompressedHeapFile(filePath string) (bool, string) {
	ext := strings.TrimPrefix(filepath.Ext(filePath), ".")

	for _, compressedExt := range compressedHeapExtensions {
		if ext == compressedExt {
			return true, ext
		}
	}
	return false, ext
}

// heapDump runs the JDK tool (jcmd, jattach, etc) to capture the heap dump to the requested file.
// The returned actualDumpPath is the actual file name written to is returned.
// In IBM JDK, this may not be the same as the requested filename for several reasons:
// - null or the empty string were specified, this will cause the JVM to write the dump to the default location based on the current dump settings and return that path.
// - Replacement (%) tokens were specified in the file name. These will have been expanded.
// - The full path is returned, if only a file name with no directory was specified the full path with the directory the dump was written to will be returned.
// - The JVM couldn't write to the specified location. In this case it will attempt to write the dump to another location, unless -Xdump:nofailover was specified on the command line.
func (t *HeapDump) heapDump(requestedFilePath string) (actualDumpPath string, err error) {
	// The default value of writtenDumpPath is the same as the requested file path
	actualDumpPath = requestedFilePath
	var output []byte

	// Heap dump: Attempt 1: jcmd
	output, err = executils.CommandCombinedOutput(executils.Command{path.Join(t.JavaHome, "/bin/jcmd"), strconv.Itoa(t.Pid), "GC.heap_dump", requestedFilePath}, executils.SudoHooker{PID: t.Pid})
	logger.Log("heap dump output from jcmd: %s, %v", output, err)
	if err != nil ||
		bytes.Index(output, []byte("No such file")) >= 0 ||
		bytes.Index(output, []byte("Permission denied")) >= 0 {
		if len(output) > 1 {
			err = fmt.Errorf("%w because %s", err, output)
		}
		var e2 error
		// Heap dump: Attempt 2a: jattach
		output, e2 = executils.CommandCombinedOutput(executils.Command{executils.Executable(), "-p", strconv.Itoa(t.Pid), "-hdPath", requestedFilePath, "-hdCaptureMode"},
			executils.EnvHooker{"pid": strconv.Itoa(t.Pid)},
			executils.SudoHooker{PID: t.Pid})
		logger.Log("heap dump output from jattach: %s, %v", output, e2)
		if e2 != nil ||
			bytes.Index(output, []byte("No such file")) >= 0 ||
			bytes.Index(output, []byte("Permission denied")) >= 0 {
			if len(output) > 1 {
				e2 = fmt.Errorf("%w because %s", e2, output)
			}
			err = fmt.Errorf("%v: %v", e2, err)
			// Heap dump: Attempt 2b: tmp jattach
			tempPath, e := executils.Copy2TempPath()
			if e != nil {
				err = fmt.Errorf("%v: %v", e, err)
				return
			}
			var e3 error
			output, e3 = executils.CommandCombinedOutput(executils.Command{tempPath, "-p", strconv.Itoa(t.Pid), "-hdPath", requestedFilePath, "-hdCaptureMode"},
				executils.EnvHooker{"pid": strconv.Itoa(t.Pid)},
				executils.SudoHooker{PID: t.Pid})
			logger.Log("heap dump output from tmp jattach: %s, %v", output, e3)
			if e3 != nil ||
				bytes.Index(output, []byte("No such file")) >= 0 ||
				bytes.Index(output, []byte("Permission denied")) >= 0 {
				if len(output) > 1 {
					e3 = fmt.Errorf("%w because %s", e3, output)
				}
				err = fmt.Errorf("%v: %v", e3, err)
				return
			}
			u, e := user.Current()
			if e != nil {
				err = fmt.Errorf("%v: %v", e, err)
				return
			}
			command := executils.Command{"sudo", "chown", fmt.Sprintf("%s:%s", u.Username, u.Username), requestedFilePath}
			e = executils.CommandRun(command)
			logger.Info().Str("cmd", strings.Join(command, " ")).Msgf("chown: %s, %v", requestedFilePath, e)
			if e != nil {
				err = fmt.Errorf("%v: %v", e, err)
				return
			}
		} else if bytes.Index(output, []byte("Dump written to")) > 0 {
			// IBM JDK jattach response:
			// Connected to remote JVM
			// Dump written to /tmp/heap_dump.out.15580.1710254434
			re := regexp.MustCompile(`(?m)^Dump written to (.*)$`)
			stringSubmatch := re.FindStringSubmatch(string(output))
			if len(stringSubmatch) > 1 {
				actualDumpPath = stringSubmatch[1]
			}
		}
		err = nil
	}
	return
}
