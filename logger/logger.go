package logger

import (
	"fmt"
	"io"
	"os"

	"shell"
)

var logger Logger

type Logger struct {
	writer io.StringWriter
}

func init() {
	logger.writer = os.Stdout
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

func SetStringWriter(writer io.StringWriter) {
	logger.writer = writer
}
