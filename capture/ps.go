package capture

import (
	"fmt"
	"io"
	"os"
	"shell"
	"shell/logger"
)

type PS struct {
	Capture
}

func NewPS() *PS {
	p := &PS{}
	return p
}

func (t *PS) Run() (result Result, err error) {
	file, err := os.Create("ps.out")
	if err != nil {
		return
	}
	defer file.Close()

	m := shell.SCRIPT_SPAN / shell.JAVACORE_INTERVAL
	for n := 1; n <= m; n++ {
		_, err = file.WriteString(fmt.Sprintf("\n%s\n", shell.NowString()))
		if err != nil {
			return
		}
		err = shell.CommandCombinedOutputToWriter(file, shell.PS)
		if err != nil {
			_, err = file.Seek(0, io.SeekStart)
			if err != nil {
				return
			}
			err = file.Truncate(0)
			if err != nil {
				return
			}
			_, err = file.Seek(0, io.SeekStart)
			if err != nil {
				return
			}
			logger.Log("trying %v, cause %v exit code != 0", shell.PS2, shell.PS)
			err = shell.CommandCombinedOutputToWriter(file, shell.PS2)
			if err != nil {
				return
			}
		}
	}
	result.Msg, result.Ok = shell.PostData(t.endpoint, "ps", file)
	return
}
