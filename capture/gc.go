package capture

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"shell"
	"shell/config"
	"shell/logger"
	"sort"
	"strconv"
	"strings"

	"github.com/mattn/go-zglob"
)

type GC struct {
	Capture
	Pid      int
	JavaHome string
	DockerID string
	GCPath   string
}

func (t *GC) Run() (result Result, err error) {
	fileName := "gc.log"
	var gcFile *os.File

	gcFile, err = processGCLogFile(t.GCPath, fileName, t.DockerID, t.Pid)
	if err != nil {
		logger.Log("process log file failed %s, err: %s", t.GCPath, err.Error())
	}

	if gcFile == nil && t.Pid > 0 {

		if gcFile == nil {
			// Garbage collection log: Attempt 5: jstat
			logger.Log("Trying to capture gc log using jstat...")
			gcFile, err = shell.CommandCombinedOutputToFile(fileName,
				shell.Command{path.Join(config.GlobalConfig.JavaHomePath, "/bin/jstat"), "-gc", "-t", strconv.Itoa(t.Pid), "2000", "30"}, shell.SudoHooker{PID: t.Pid})
			if err != nil {
				logger.Log("jstat failed cause %s", err.Error())
			}
		}
		if gcFile == nil {
			// Garbage collection log: Attempt 6a: jattach
			logger.Log("Trying to capture gc log using jattach...")
			gcFile, err = shell.CommandCombinedOutputToFile(fileName,
				shell.Command{shell.Executable(), "-p", strconv.Itoa(t.Pid), "-gcCaptureMode"}, shell.EnvHooker{"pid": strconv.Itoa(t.Pid)}, shell.SudoHooker{PID: t.Pid})
			if err != nil {
				logger.Log("jattach failed cause %s", err.Error())
			}
		}
		if gcFile == nil {
			// Garbage collection log: Attempt 6b: tmp jattach
			logger.Log("Trying to capture gc log using tmp jattach...")
			var tempPath string
			tempPath, err = shell.Copy2TempPath()
			if err != nil {
				return
			}
			gcFile, err = shell.CommandCombinedOutputToFile(fileName,
				shell.Command{tempPath, "-p", strconv.Itoa(t.Pid), "-gcCaptureMode"}, shell.EnvHooker{"pid": strconv.Itoa(t.Pid)}, shell.SudoHooker{PID: t.Pid})
			if err != nil {
				logger.Log("tmp jattach failed cause %s", err.Error())
			}
		}

		if gcFile != nil {
			t.GCPath = fileName
			logger.Log("gc log set to %s", t.GCPath)
		}
	}

	if gcFile != nil {
		defer func() {
			_ = gcFile.Close()
		}()
	}

	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "gc", gcFile)
	absGCPath, err := filepath.Abs(t.GCPath)
	if err != nil {
		absGCPath = fmt.Sprintf("path %s: %s", t.GCPath, err.Error())
	}
	result.Msg += "\n\nGC Path: " + absGCPath
	return
}

func processGCLogFile(gcPath string, out string, dockerID string, pid int) (gc *os.File, err error) {
	if len(gcPath) <= 0 {
		return
	}
	// -Xloggc:/app/boomi/gclogs/gc%t.log
	if strings.Contains(gcPath, `%t`) {
		pattern := strings.ReplaceAll(gcPath, "%t", "*")
		files, err := zglob.Glob(pattern)

		if err != nil {
			logger.Log("error on expanding %%t, pattern:%s, err:%s", pattern, err)
		}

		// descending
		sort.Slice(files, func(i, j int) bool {
			fileNameI := filepath.Base(files[i])
			fileNameJ := filepath.Base(files[j])
			return strings.Compare(fileNameI, fileNameJ) > 0
		})

		if len(files) > 0 {
			logger.Log("gcPath is updated from %s to %s", gcPath, files[0])
			gcPath = files[0]
		}
	}

	if strings.Contains(gcPath, `%pid`) {
		gcPath = strings.Replace(gcPath, `%pid`, ""+strconv.Itoa(pid), 1)

		if !fileExists(gcPath) && strings.Contains(gcPath, ",") {
			splitByComma := strings.Split(gcPath, ",")
			// Check if it's in the form of filename,x,y
			// Get the last modified file of files in the filename.* pattern.
			if len(splitByComma) == 3 {
				originalGcPath := splitByComma[0]
				gcPath = findLatestFileInRotatingLogFiles(originalGcPath)

				if originalGcPath != gcPath {
					logger.Log("resolved last modified file of gc files: %s", gcPath)
				}
			}
		}
	}

	// -Xloggc:/home/ec2-user/buggyapp/gc.%p.log
	// /home/ec2-user/buggyapp/gc.2843.log
	// or
	// /home/ec2-user/buggyapp/gc.pid2843.log
	if strings.Contains(gcPath, `%p`) {
		originalGcPath := gcPath
		gcPath = strings.Replace(originalGcPath, `%p`, ""+strconv.Itoa(pid), 1)
		logger.Log("trying to use gcPath %s", gcPath)

		if !fileExists(gcPath) {
			logger.Log("gcPath %s doesn't exist", gcPath)

			gcPath = strings.Replace(originalGcPath, `%p`, "pid"+strconv.Itoa(pid), 1)
			logger.Log("trying to use gcPath %s", gcPath)
		}
	}

	// Attempt 1 to find the latest file in rotating gc logs
	originalGcPath := gcPath
	gcPath = findLatestFileInRotatingLogFiles(originalGcPath)

	if originalGcPath != gcPath {
		logger.Log("resolved last modified file of gc files: %s", gcPath)
	}

	if len(dockerID) > 0 {
		err = shell.DockerCopy(out, dockerID+":"+gcPath)
		if err == nil {
			gc, err = os.Open(out)
			return
		}
	} else {
		gc, err = os.Create(out)
		if err != nil {
			return
		}
		err = copyFile(gc, gcPath, pid)
		if err == nil {
			return
		}
	}

	// Attempt 2 to find the latest file in rotating gc logs
	logger.Log("collecting rotation gc logs, because file open failed %s", err.Error())

	// err is other than not exists
	if !os.IsNotExist(err) {
		return
	}

	// config.GlobalConfig.GCPath is not exists, maybe using -XX:+UseGCLogFileRotation
	d := filepath.Dir(gcPath)
	logName := filepath.Base(gcPath)
	var fs []string
	if len(dockerID) > 0 {
		output, err := shell.DockerExecute(dockerID, "ls", "-1", d)
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(bytes.NewReader(output))
		for scanner.Scan() {
			line := scanner.Text()
			line = strings.TrimSpace(line)
			fs = append(fs, line)
		}
	} else {
		open, err := os.Open(d)
		if err != nil {
			return nil, err
		}
		defer open.Close()
		fs, err = open.Readdirnames(0)
		if err != nil {
			return nil, err
		}
	}
	re := regexp.MustCompile(logName + "\\.([0-9]+?)\\.current")
	reo := regexp.MustCompile(logName + "\\.([0-9]+)")
	var rf []string
	files := make([]int, 0, len(fs))
	for _, f := range fs {
		r := re.FindStringSubmatch(f)
		if len(r) > 1 {
			rf = r
			continue
		}
		r = reo.FindStringSubmatch(f)
		if len(r) > 1 {
			p, err := strconv.Atoi(r[1])
			if err != nil {
				logger.Log("skipped file %s because can not parse its index", f)
				continue
			}
			files = append(files, p)
		}
	}
	if len(rf) < 2 {
		err = fmt.Errorf("can not find the current log file, %w", os.ErrNotExist)
		return
	}
	p, err := strconv.Atoi(rf[1])
	if err != nil {
		return
	}
	// try to find previous log
	var preLog string
	if len(files) == 1 {
		preLog = gcPath + "." + strconv.Itoa(files[0])
	} else if len(files) > 1 {
		files = append(files, p)
		sort.Ints(files)
		index := -1
		for i, file := range files {
			if file == p {
				index = i
				break
			}
		}
		if index >= 0 {
			if index-1 >= 0 {
				preLog = gcPath + "." + strconv.Itoa(files[index-1])
			} else {
				preLog = gcPath + "." + strconv.Itoa(files[len(files)-1])
			}
		}
	}
	if gc == nil {
		gc, err = os.Create(out)
		if err != nil {
			return
		}
	}
	if len(preLog) > 0 {
		logger.Log("collecting previous gc log %s", preLog)
		if len(dockerID) > 0 {
			tmp := filepath.Join(os.TempDir(), out+".pre")
			err = shell.DockerCopy(tmp, dockerID+":"+preLog)
			if err == nil {
				err = copyFile(gc, tmp, pid)
			}
		} else {
			err = copyFile(gc, preLog, pid)
		}
		if err != nil {
			logger.Log("failed to collect previous gc log %s", err.Error())
		} else {
			logger.Log("collected previous gc log %s", preLog)
		}
	}

	curLog := filepath.Join(d, rf[0])
	logger.Log("collecting previous gc log %s", curLog)
	if len(dockerID) > 0 {
		tmp := filepath.Join(os.TempDir(), out+".cur")
		err = shell.DockerCopy(tmp, dockerID+":"+curLog)
		if err == nil {
			err = copyFile(gc, tmp, pid)
		}
	} else {
		err = copyFile(gc, curLog, pid)
	}
	if err != nil {
		logger.Log("failed to collect previous gc log %s", err.Error())
	} else {
		logger.Log("collected previous gc log %s", curLog)
	}
	return
}

// combine previous gc log to new gc log
func copyFile(gc *os.File, file string, pid int) (err error) {
	log, err := os.Open(file)
	if err != nil && runtime.GOOS == "linux" {
		logger.Log("Failed to %s. Trying to open in the Docker container...", err)
		log, err = os.Open(filepath.Join("/proc", strconv.Itoa(pid), "root", file))
	}
	if err != nil {
		return
	}
	defer log.Close()
	_, err = io.Copy(gc, log)
	return
}

func fileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func findLatestFileInRotatingLogFiles(gcPath string) string {
	pattern := gcPath + ".*"
	matches, err := zglob.Glob(pattern)
	if fileExists(gcPath) {
		matches = append(matches, gcPath)
	}

	if err != nil {
		logger.Log(err.Error())
	} else {
		fileInfos := []os.FileInfo{}

		for _, match := range matches {
			fileInfo, err := os.Lstat(match)
			if err != nil {
				logger.Log(err.Error())
			}
			fileInfos = append(fileInfos, fileInfo)
		}

		// descending
		sort.Slice(fileInfos, func(i, j int) bool {
			return fileInfos[i].ModTime().After(fileInfos[j].ModTime())
		})

		if len(fileInfos) > 0 {
			gcPath = filepath.Join(filepath.Dir(gcPath), fileInfos[0].Name())
		}
	}

	return gcPath
}
