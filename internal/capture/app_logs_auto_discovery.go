package capture

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"
	"yc-agent/internal/logger"
)

// logPatterns contains precompiled regex patterns so they are compiled once at startup rather than each function call.
var logPatterns = []*regexp.Regexp{
	regexp.MustCompile(`.*\.log$`),    // Matches *.log
	regexp.MustCompile(`.*log.*\..*`), // Matches *log*.*
}

// binaryFileExtensions are never auto-discovered, even when the name matches logPatterns
// (e.g. log4j-core-2.19.0.jar). Includes rotated logs like app.log.gz; configured appLogs are unaffected.
var binaryFileExtensions = map[string]struct{}{
	".jar": {}, ".war": {}, ".ear": {}, ".zip": {}, ".gz": {}, ".tgz": {}, ".bz2": {}, ".xz": {}, ".7z": {},
	".class": {}, ".so": {}, ".dylib": {}, ".dll": {}, ".jnilib": {}, ".jsa": {},
	".db": {}, ".dat": {}, ".idx": {}, ".lck": {},
}

// binaryFileSignatures are magic numbers of archives and binaries. Checked on the head because
// a zip can end with an ASCII archive comment.
var binaryFileSignatures = [][]byte{
	{'P', 'K', 0x03, 0x04},   // zip, jar, war, ear
	{0x1f, 0x8b},             // gzip
	{0x7f, 'E', 'L', 'F'},    // ELF
	{0xfe, 0xed, 0xfa, 0xce}, // Mach-O 32-bit
	{0xfe, 0xed, 0xfa, 0xcf}, // Mach-O 64-bit
	{0xce, 0xfa, 0xed, 0xfe}, // Mach-O 32-bit, reverse byte order
	{0xcf, 0xfa, 0xed, 0xfe}, // Mach-O 64-bit, reverse byte order
	{0xca, 0xfe, 0xba, 0xbe}, // Java class file, Mach-O universal binary
}

// headSampleLen is the length of the longest binaryFileSignatures entry.
var headSampleLen = func() int64 {
	longest := 0
	for _, signature := range binaryFileSignatures {
		longest = max(longest, len(signature))
	}
	return int64(longest)
}()

// tailSampleLen is how many trailing bytes are read for the NUL and ASCII checks.
const tailSampleLen = 1000

// DiscoverOpenedLogFilesByProcess returns a list of file paths for log files that are
// opened by the given process identified by pid. A file is considered a log file if:
// - its name matches any of the precompiled log patterns and has no known binary/archive extension,
// - it is a regular file (see isRegularFile),
// - its content looks like a text log (see looksLikeTextLog).
//
// Each path is returned at most once, even if the process holds several descriptors to it.
//
// If the runtime is not Linux, it returns an empty slice with no error.
func DiscoverOpenedLogFilesByProcess(pid int) ([]string, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return []string{}, nil
	}

	openedFiles, err := GetOpenedFilesByProcess(pid)
	openedLogFiles := []string{}

	if err != nil {
		return nil, err
	}

	// One entry per descriptor, already resolved to a path.
	seen := make(map[string]struct{}, len(openedFiles))

	for _, filePath := range openedFiles {
		logger.Debug().Msgf("DiscoverOpenedLogFilesByProcess: opened file by process (pid=%d): %s", pid, filePath)

		if _, ok := seen[filePath]; ok {
			continue
		}
		seen[filePath] = struct{}{}

		fileBaseName := filepath.Base(filePath)
		if !isLogFileName(fileBaseName) || !isRegularFile(filePath) {
			continue
		}

		head, tail, err := sampleFile(filePath, headSampleLen, tailSampleLen)
		if err != nil {
			continue
		}

		if looksLikeTextLog(head, tail) {
			openedLogFiles = append(openedLogFiles, filePath)
		}
	}

	return openedLogFiles, nil
}

// isLogFileName checks if the filename looks like a log file: it matches a log pattern and
// does not have a known binary/archive extension.
func isLogFileName(s string) bool {
	return matchLogPattern(s) && !hasBinaryFileExtension(s)
}

// hasBinaryFileExtension checks s against binaryFileExtensions, ignoring case.
func hasBinaryFileExtension(s string) bool {
	_, ok := binaryFileExtensions[strings.ToLower(filepath.Ext(s))]
	return ok
}

// matchLogPattern checks if the filename matches any of the precompiled log patterns.
func matchLogPattern(s string) bool {
	for _, pattern := range logPatterns {
		if pattern.MatchString(s) {
			return true
		}
	}
	return false
}

// maxNULShare is the NUL byte share above which a tail is binary. Jar tails hold 148-512 NULs
// per 1000 bytes (a one-entry zip 54), while a log may carry a few stray NULs.
const maxNULShare = 0.01

// looksLikeTextLog checks if a file's content looks like a text log, given its first bytes (head) and last bytes (tail):
//   - head does not start with a known binary signature,
//   - tail is not dense with NUL bytes (see maxNULShare): every zip ends with central directory headers and an
//     end-of-central-directory record full of zero fields,
//   - tail is mostly ASCII.
//
// NUL bytes are checked in the tail only: copytruncate rotation leaves NUL runs at the head of genuine logs.
func looksLikeTextLog(head, tail []byte) bool {
	return !hasBinarySignature(head) && !hasManyNULs(tail) && IsMostlyASCII(tail)
}

// hasManyNULs reports whether more than maxNULShare of b is NUL bytes.
func hasManyNULs(b []byte) bool {
	return float64(bytes.Count(b, []byte{0})) > maxNULShare*float64(len(b))
}

// hasBinarySignature reports whether b starts with any of binaryFileSignatures.
func hasBinarySignature(b []byte) bool {
	for _, signature := range binaryFileSignatures {
		if bytes.HasPrefix(b, signature) {
			return true
		}
	}
	return false
}

// isRegularFile reports whether path is a regular file. Opening or reading a FIFO can block.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// sampleFile returns up to the first headLen and last tailLen bytes of filename, read through one descriptor.
func sampleFile(filename string, headLen, tailLen int64) ([]byte, []byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}

	head, err := io.ReadAll(io.NewSectionReader(file, 0, headLen))
	if err != nil {
		return nil, nil, err
	}

	tail, err := readTail(file, info.Size(), tailLen)
	if err != nil {
		return nil, nil, err
	}

	return head, tail, nil
}

// readTail returns up to the last n bytes of r, measured at size. Returns only the bytes read,
// in case the file shrank since.
func readTail(r io.ReaderAt, size, n int64) ([]byte, error) {
	buf := make([]byte, min(n, size))
	read, err := r.ReadAt(buf, size-int64(len(buf)))
	if err != nil && err != io.EOF {
		return nil, err
	}

	return buf[:read], nil
}

// IsMostlyASCII determines if more than 70% of the bytes in b are ASCII.
// It returns true if the proportion of ASCII characters is greater than 0.7, and false otherwise.
func IsMostlyASCII(b []byte) bool {
	ASCIICount := 0

	for i := 0; i < len(b); i++ {
		if b[i] >= 32 && b[i] < unicode.MaxASCII {
			ASCIICount++
		}
	}

	return float64(ASCIICount)/float64(len(b)) > 0.7
}
