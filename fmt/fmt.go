package fmt

import (
	"fmt"
	"io"
	"os"
)

func Println(a ...interface{}) (n int, err error) {
	return fmt.Fprintln(os.Stderr, a...)
}

func Printf(format string, a ...interface{}) (n int, err error) {
	return fmt.Fprintf(os.Stderr, format, a...)
}

var stdOutAndErr = io.MultiWriter(os.Stderr, os.Stdout)

func Printfx(format string, a ...interface{}) (n int, err error) {
	return fmt.Fprintf(stdOutAndErr, format, a...)
}

var Sprintf = fmt.Sprintf
var Errorf = fmt.Errorf
