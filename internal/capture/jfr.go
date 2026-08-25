package capture

import (
	"fmt"
	"os"
	"path"
	"strconv"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

// JFRRecordingName identifies the recording started by StartJFR.
const JFRRecordingName = "ycJFR"

// JFRFileName is the fixed filename yc-agent uses for the JFR recording it
// captures in -onlyCapture mode.
const JFRFileName = "my.jfr"

// JFRCaptureDuration is how long a JFR recording runs for. It's passed as
// JFR.start's own duration= option, so the JVM auto-stops the recording (and
// flushes/finalizes the file) on its own - no explicit JFR.stop call needed.
// The caller (ondemand.FullCapture) waits out this same duration before
// calling TransmitJFR, in case the rest of the capture finishes sooner, so
// the file is guaranteed to be finalized by the time it's read.
const JFRCaptureDuration = 60 * time.Second

// StartJFR starts a JFR recording on the target JVM (pid), writing to
// filePath. filePath must be an absolute path (or, for a dockerized target, a
// path inside the container) since the target JVM's own working directory is
// generally not the same as yc-agent's. The recording auto-stops after
// JFRCaptureDuration; see TransmitJFR.
func StartJFR(pid int, javaHome, filePath string) error {
	cmd := fmt.Sprintf("JFR.start name=%s filename=%s duration=%ds", JFRRecordingName, filePath, int(JFRCaptureDuration.Seconds()))
	return runJcmd(pid, javaHome, cmd)
}

// runJcmd runs a jcmd diagnostic command against pid, trying the JDK's jcmd
// binary first and falling back to jattach, then tmp jattach (mirrors
// HDSub.executeJcmd).
func runJcmd(pid int, javaHome, command string) error {
	out, err := executils.CommandCombinedOutput(
		executils.Command{path.Join(javaHome, "bin/jcmd"), strconv.Itoa(pid), command},
		executils.SudoHooker{PID: pid})
	if err == nil {
		logger.Log("jcmd %s: %s", command, out)
		return nil
	}

	logger.Log("jcmd failed (%v), falling back to jattach for: %s", err, command)

	out, err = executils.CommandCombinedOutput(
		executils.Command{executils.Executable(), "-p", strconv.Itoa(pid), "-jCmdCaptureMode", command},
		executils.EnvHooker{"pid": strconv.Itoa(pid)}, executils.SudoHooker{PID: pid})
	if err == nil {
		logger.Log("jattach %s: %s", command, out)
		return nil
	}

	logger.Log("jattach failed (%v), falling back to tmp jattach for: %s", err, command)

	tempPath, tmpErr := executils.Copy2TempPath()
	if tmpErr != nil {
		return fmt.Errorf("failed to run %q: %w (tmp jattach fallback failed: %v)", command, err, tmpErr)
	}

	out, err = executils.CommandCombinedOutput(
		executils.Command{tempPath, "-p", strconv.Itoa(pid), "-jCmdCaptureMode", command},
		executils.EnvHooker{"pid": strconv.Itoa(pid)}, executils.SudoHooker{PID: pid})
	if err != nil {
		return fmt.Errorf("failed to run %q: %w, output: %s", command, err, out)
	}
	logger.Log("tmp jattach %s: %s", command, out)
	return nil
}

// TransmitJFR transmits the JFR recording started by StartJFR (filePath, the
// same path passed to it) to endpoint with dt=jfr. It assumes the recording
// has already auto-stopped (StartJFR's duration= option) by the time this
// runs. If dockerID is set, the recording is copied out of the container to
// the local, relative JFRFileName first (so it also ends up in the local
// capture bundle, consistent with how GC logs are handled).
//
// The transmission bypasses the -onlyCapture short-circuit in PostData, since
// the recording needs to reach the yc-receiver regardless of -onlyCapture.
func TransmitJFR(endpoint, filePath, dockerID string) (msg string, ok bool) {
	localPath := filePath
	if len(dockerID) > 0 {
		localPath = JFRFileName
		if err := DockerCopy(localPath, dockerID+":"+filePath); err != nil {
			return fmt.Sprintf("failed to copy JFR recording out of container: %s", err.Error()), false
		}
	}

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Sprintf("failed to open JFR recording %s: %s", localPath, err.Error()), false
	}
	defer file.Close()

	return PostDataForce(endpoint, "jfr", file)
}
