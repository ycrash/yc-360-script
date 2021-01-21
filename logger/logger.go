package logger

import (
	"io"
	"os"
	"time"

	rotatelogs "github.com/lestrrat-go/file-rotatelogs"
	"github.com/rs/zerolog"
)

var logger zerolog.Logger
var stdLogger zerolog.Logger

func Init(path string, count uint, size int64, logLevel string) (err error) {
	level, err := zerolog.ParseLevel(logLevel)
	if err != nil {
		return
	}
	var logWriter io.Writer
	var stdout io.Writer = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.UnixDate}
	if len(path) > 0 {
		f, err := rotatelogs.New(
			path+".%Y%m%d%H%M",
			rotatelogs.WithLinkName(path),
			rotatelogs.WithRotationCount(count),
			rotatelogs.WithRotationSize(size),
		)
		if err != nil {
			return err
		}
		logWriter = zerolog.ConsoleWriter{Out: f, TimeFormat: time.UnixDate, NoColor: true}
	} else {
		logWriter = zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.UnixDate}
	}
	logger = zerolog.New(logWriter).With().Timestamp().Logger().Level(level)
	stdLogger = zerolog.New(stdout).With().Timestamp().Logger().Level(level)
	return
}

func Log(format string, values ...interface{}) {
	logger.Info().Msgf(format, values...)
}

func StdLog(format string, values ...interface{}) {
	stdLogger.Info().Msgf(format, values...)
}
