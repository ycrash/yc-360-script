package logger

import (
	"io"
	"os"
	"time"

	rotatelogs "github.com/lestrrat-go/file-rotatelogs"
	"github.com/rs/zerolog"
)

var Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.UnixDate}).With().Timestamp().Logger()
var stdLogger zerolog.Logger
var Log2File bool

func Init(path string, count uint, size int64, logLevel string) (err error) {
	level, err := zerolog.ParseLevel(logLevel)
	if err != nil {
		return
	}
	var logWriter io.Writer
	var stdout io.Writer = zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.UnixDate}
	if len(path) > 0 {
		Log2File = true
		f, err := rotatelogs.New(
			path+".%Y%m%d",
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
	Logger = zerolog.New(logWriter).With().Timestamp().Logger().Level(level)
	stdLogger = zerolog.New(stdout).With().Timestamp().Logger().Level(level)
	return
}

func Log(format string, values ...interface{}) {
	Logger.Info().Msgf(format, values...)
}

func StdLog(format string, values ...interface{}) {
	stdLogger.Info().Msgf(format, values...)
}

// Output duplicates the global logger and sets w as its output.
func Output(w io.Writer) zerolog.Logger {
	return Logger.Output(w)
}

// With creates a child logger with the field added to its context.
func With() zerolog.Context {
	return Logger.With()
}

// Level creates a child logger with the minimum accepted level set to level.
func Level(level zerolog.Level) zerolog.Logger {
	return Logger.Level(level)
}

// Sample returns a logger with the s sampler.
func Sample(s zerolog.Sampler) zerolog.Logger {
	return Logger.Sample(s)
}

// Hook returns a logger with the h Hook.
func Hook(h zerolog.Hook) zerolog.Logger {
	return Logger.Hook(h)
}

// Err starts a new message with error level with err as a field if not nil or
// with info level if err is nil.
//
// You must call Msg on the returned event in order to send the event.
func Err(err error) *zerolog.Event {
	return Logger.Err(err)
}

// Trace starts a new message with trace level.
//
// You must call Msg on the returned event in order to send the event.
func Trace() *zerolog.Event {
	return Logger.Trace()
}

// Debug starts a new message with debug level.
//
// You must call Msg on the returned event in order to send the event.
func Debug() *zerolog.Event {
	return Logger.Debug()
}

// Info starts a new message with info level.
//
// You must call Msg on the returned event in order to send the event.
func Info() *zerolog.Event {
	return Logger.Info()
}

// Warn starts a new message with warn level.
//
// You must call Msg on the returned event in order to send the event.
func Warn() *zerolog.Event {
	return Logger.Warn()
}

// Error starts a new message with error level.
//
// You must call Msg on the returned event in order to send the event.
func Error() *zerolog.Event {
	return Logger.Error()
}

// Fatal starts a new message with fatal level. The os.Exit(1) function
// is called by the Msg method.
//
// You must call Msg on the returned event in order to send the event.
func Fatal() *zerolog.Event {
	return Logger.Fatal()
}

// Panic starts a new message with panic level. The message is also sent
// to the panic function.
//
// You must call Msg on the returned event in order to send the event.
func Panic() *zerolog.Event {
	return Logger.Panic()
}

// WithLevel starts a new message with level.
//
// You must call Msg on the returned event in order to send the event.
func WithLevel(level zerolog.Level) *zerolog.Event {
	return Logger.WithLevel(level)
}
