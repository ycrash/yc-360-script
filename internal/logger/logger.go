package logger

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	rotatelogs "github.com/lestrrat-go/file-rotatelogs"
	"github.com/rs/zerolog"
)

type stackElement struct {
	l *zerolog.Logger
	f *os.File
}

var (
	logger        atomic.Value
	loggerMtx     sync.Mutex
	loggerStack   []stackElement
	rootLogWriter io.Writer

	stdLogger zerolog.Logger
	Log2File  bool
)

func init() {
	w := zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.UnixDate}
	rootLogWriter = w
	l := zerolog.New(w).With().Timestamp().Logger()
	logger.Store(&l)
}

func GetLogger() *zerolog.Logger {
	return logger.Load().(*zerolog.Logger)
}

func SetLogger(l *zerolog.Logger) {
	logger.Store(l)
}

func StartWritingToFile(file string) (f *os.File, err error) {
	loggerMtx.Lock()
	defer loggerMtx.Unlock()

	f, err = os.Create(file)
	if err != nil {
		return
	}
	lw := zerolog.ConsoleWriter{Out: f, TimeFormat: time.UnixDate, NoColor: true}
	l := GetLogger()
	loggerStack = append(loggerStack, stackElement{l, f})
	w := io.MultiWriter(lw, rootLogWriter)
	nl := l.Output(w)
	SetLogger(&nl)
	return
}

func StopWritingToFile() (err error) {
	loggerMtx.Lock()
	defer loggerMtx.Unlock()

	l := loggerStack[len(loggerStack)-1]
	loggerStack = loggerStack[:len(loggerStack)-1]
	SetLogger(l.l)
	err = l.f.Close()
	if errors.Is(err, os.ErrClosed) {
		err = nil
	}
	return
}

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
	rootLogWriter = logWriter
	l := zerolog.New(logWriter).With().Timestamp().Logger().Level(level)
	SetLogger(&l)
	stdLogger = zerolog.New(stdout).With().Timestamp().Logger().Level(level)
	return
}

func Log(format string, values ...interface{}) {
	GetLogger().Info().Msgf(format, values...)
}

func StdLog(format string, values ...interface{}) {
	stdLogger.Info().Msgf(format, values...)
}

// Output duplicates the global logger and sets w as its output.
func Output(w io.Writer) zerolog.Logger {
	return GetLogger().Output(w)
}

// With creates a child logger with the field added to its context.
func With() zerolog.Context {
	return GetLogger().With()
}

// Level creates a child logger with the minimum accepted level set to level.
func Level(level zerolog.Level) zerolog.Logger {
	return GetLogger().Level(level)
}

// Sample returns a logger with the s sampler.
func Sample(s zerolog.Sampler) zerolog.Logger {
	return GetLogger().Sample(s)
}

// Hook returns a logger with the h Hook.
func Hook(h zerolog.Hook) zerolog.Logger {
	return GetLogger().Hook(h)
}

// Err starts a new message with error level with err as a field if not nil or
// with info level if err is nil.
//
// You must call Msg on the returned event in order to send the event.
func Err(err error) *zerolog.Event {
	return GetLogger().Err(err)
}

// Trace starts a new message with trace level.
//
// You must call Msg on the returned event in order to send the event.
func Trace() *zerolog.Event {
	return GetLogger().Trace()
}

// Debug starts a new message with debug level.
//
// You must call Msg on the returned event in order to send the event.
func Debug() *zerolog.Event {
	return GetLogger().Debug()
}

// Info starts a new message with info level.
//
// You must call Msg on the returned event in order to send the event.
func Info() *zerolog.Event {
	return GetLogger().Info()
}

// Warn starts a new message with warn level.
//
// You must call Msg on the returned event in order to send the event.
func Warn() *zerolog.Event {
	return GetLogger().Warn()
}

// Error starts a new message with error level.
//
// You must call Msg on the returned event in order to send the event.
func Error() *zerolog.Event {
	return GetLogger().Error()
}

// Fatal starts a new message with fatal level. The os.Exit(1) function
// is called by the Msg method.
//
// You must call Msg on the returned event in order to send the event.
func Fatal() *zerolog.Event {
	return GetLogger().Fatal()
}

// Panic starts a new message with panic level. The message is also sent
// to the panic function.
//
// You must call Msg on the returned event in order to send the event.
func Panic() *zerolog.Event {
	return GetLogger().Panic()
}

// WithLevel starts a new message with level.
//
// You must call Msg on the returned event in order to send the event.
func WithLevel(level zerolog.Level) *zerolog.Event {
	return GetLogger().WithLevel(level)
}
