package logger

import (
	"fmt"
	"io"
	"os"

	"shell"
)

var logger Logger

type Writer interface {
	io.StringWriter
	io.Writer
}

type Logger struct {
	writer Writer
}

func init() {
	logger.writer = os.Stderr
}

func (logger *Logger) Log(format string, values ...interface{}) {
	stamp := shell.NowString()
	if len(values) == 0 {
		logger.writer.WriteString(stamp + format + "\n")
		return
	}
	logger.writer.WriteString(stamp + fmt.Sprintf(format, values...) + "\n")
}

func Log(format string, values ...interface{}) {
	logger.Log(format, values...)
}

func SetWriter(writer Writer) {
	logger.writer = writer
}

func GetWriter() (writer io.Writer) {
	return logger.writer
}
