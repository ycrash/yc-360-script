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
	"time"
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
		d := filepath.Dir(gcPath)
		open, err := os.Open(d)
		if err != nil {
			return nil, err
		}
		defer open.Close()
		fs, err := open.Readdirnames(0)
		if err != nil {
			return nil, err
		}

		var t time.Time
		var tf string
		for _, f := range fs {
			stat, err := os.Stat(filepath.Join(d, f))
			if err != nil {
				continue
			}
			mt := stat.ModTime()
			if t.IsZero() || mt.After(t) {
				t = mt
				tf = f
			}
		}
		if len(tf) > 0 {
			gcPath = filepath.Join(d, tf)
		}
	}
	// -Xloggc:/home/ec2-user/buggyapp/gc.%p.log
	// /home/ec2-user/buggyapp/gc.pid2843.log
	if strings.Contains(gcPath, `%p`) {
		gcPath = strings.Replace(gcPath, `%p`, ""+strconv.Itoa(pid), 1)
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
