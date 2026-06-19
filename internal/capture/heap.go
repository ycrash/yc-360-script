package capture

import (
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
	"yc-agent/internal/config"
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
	"zst",
	"zstd",
	"bz2",
	"tgz",
	"tar",
	"tar.gz",
}

const hdOut = "heap_dump.out"

// compressedHeapContentEncoding reports whether path already has a recognized
// compressed-archive extension and returns the server encoding token for it.
func compressedHeapContentEncoding(path string) (string, bool) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	for _, compressedExt := range compressedHeapExtensions {
		if ext == compressedExt {
			if ext == "zstd" {
				return "zst", true
			}
			return ext, true
		}
	}
	return "", false
}

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

func (t *HeapDump) Run() (Result, error) {
	// Acquire the source heap dump
	var srcPath string
	var srcFile *os.File

	if len(t.hdPath) > 0 {
		srcPath = t.hdPath

		hd, err := os.Open(srcPath)

		// Fallback, try to open the file in the Docker container
		if err != nil && runtime.GOOS == "linux" {
			logger.Log("failed to open hdPath(%s) err: %s. Trying to open in the Docker container...", t.hdPath, err.Error())
			hd, err = os.Open(filepath.Join("/proc", strconv.Itoa(t.Pid), "root", t.hdPath))
		}

		if err != nil {
			logger.Log("failed to open hdPath(%s) err: %s", t.hdPath, err.Error())
			return Result{
				Msg: fmt.Sprintf("failed to open heap dump file: %s", err.Error()),
				Ok:  false,
			}, err
		}

		srcFile = hd

		// Ensure the source heap dump file is closed when the function exits
		defer func() {
			if err := hd.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				logger.Log("failed to close source heap dump file: %s", err.Error())
			}
		}()
	} else if t.Pid > 0 && t.dump {
		hd, actualDumpPath, err := t.captureDumpFile()
		if err != nil {
			return Result{
				Msg: fmt.Sprintf("capture heap dump failed: %s", err.Error()),
				Ok:  false,
			}, nil
		}

		srcPath = actualDumpPath
		srcFile = hd

		// Close before removing the captured raw dump.
		defer func() {
			if err := hd.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				logger.Log("failed to close captured heap dump file: %s", err.Error())
			}

			if removeErr := os.Remove(actualDumpPath); removeErr != nil {
				logger.Log("failed to rm hd file %s cause err: %s", actualDumpPath, removeErr.Error())
			}
		}()
	}

	if srcFile == nil {
		return Result{Msg: "skipped heap dump"}, nil
	}

	// Copy the source dump into the capture directory.
	contentEncoding, srcCompressed := compressedHeapContentEncoding(srcPath)
	dstPath := hdOut
	if srcCompressed {
		srcExt := strings.TrimPrefix(filepath.Ext(srcPath), ".")
		dstPath = "heap_dump." + srcExt
	}

	logger.Log("copying heap dump data %s", t.hdPath)

	dstFile, err := os.OpenFile(dstPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return Result{
			Msg: fmt.Sprintf("failed creating heap dump in current working directory: %s", err.Error()),
			Ok:  false,
		}, nil
	}

	// Ensure the file is closed when the function exits
	defer func() {
		if err := dstFile.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			logger.Log("failed to close destination heap dump file: %s", err.Error())
		}
	}()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return Result{
			Msg: fmt.Sprintf("failed copying heap dump data: %s", err.Error()),
			Ok:  false,
		}, nil
	}

	// Sync the file to ensure all data is written to disk
	if err := dstFile.Sync(); err != nil {
		return Result{
			Msg: fmt.Sprintf("failed syncing heap dump file: %s", err.Error()),
			Ok:  false,
		}, nil
	}

	logger.Log("copied heap dump data %s to %s", t.hdPath, dstPath)

	if srcCompressed {
		return t.UploadCapturedFileAlreadyCompressed(dstFile, contentEncoding), nil
	}

	return t.UploadCapturedFile(dstFile), nil
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

func (t *HeapDump) UploadCapturedFileAlreadyCompressed(file *os.File, contentEncoding string) Result {
	// 0 timeout = no timeout
	msg, ok := PostDataWithTimeout(t.Endpoint(), fmt.Sprintf("hd&Content-Encoding=%s", contentEncoding), file, 0*time.Second)

	return Result{
		Msg: msg,
		Ok:  ok,
	}
}

// UploadCapturedFile zstd-compresses the raw heap dump on the fly and uploads it
// as Content-Encoding=zst.
func (t *HeapDump) UploadCapturedFile(file *os.File) Result {
	if config.GlobalConfig.OnlyCapture {
		return Result{Msg: "in only capture mode"}
	}
	if file == nil {
		return Result{Msg: "file is not captured"}
	}
	stat, err := file.Stat()
	if err != nil {
		return Result{Msg: fmt.Sprintf("file stat err %s", err.Error())}
	}
	fileName := stat.Name()
	if stat.Size() < 1 {
		return Result{Msg: fmt.Sprintf("skipped empty file %s", fileName)}
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Result{
			Msg: fmt.Sprintf("failed seeking to beginning of heap dump file: %s", err.Error()),
			Ok:  false,
		}
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})

	go func() {
		defer close(done)

		enc, err := newZstdEncoder(pw)
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		_, copyErr := io.Copy(enc, file)
		closeErr := enc.Close()
		if copyErr == nil {
			copyErr = closeErr
		}

		pw.CloseWithError(copyErr)
	}()

	msg, ok := PostReaderWithTimeout(t.Endpoint(), "hd&Content-Encoding=zst", pr, 0*time.Second)

	pr.CloseWithError(io.ErrClosedPipe)
	<-done

	return Result{
		Msg: msg,
		Ok:  ok,
	}
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
		bytes.Contains(output, []byte("No such file")) ||
		bytes.Contains(output, []byte("Permission denied")) {
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
			bytes.Contains(output, []byte("No such file")) ||
			bytes.Contains(output, []byte("Permission denied")) {
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
				bytes.Contains(output, []byte("No such file")) ||
				bytes.Contains(output, []byte("Permission denied")) {
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
