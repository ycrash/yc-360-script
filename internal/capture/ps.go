package capture

import (
	"fmt"
	"io"
	"os"

	"shell/internal/capture/executils"
	"shell/internal/logger"
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

	m := executils.SCRIPT_SPAN / executils.JAVACORE_INTERVAL
	for n := 1; n <= m; n++ {
		_, err = file.WriteString(fmt.Sprintf("\n%s\n", executils.NowString()))
		if err != nil {
			return
		}
		err = executils.CommandCombinedOutputToWriter(file, executils.PS)
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
			logger.Log("trying %v, cause %v exit code != 0", executils.PS2, executils.PS)
			err = executils.CommandCombinedOutputToWriter(file, executils.PS2)
			if err != nil {
				return
			}
		}
	}
	result.Msg, result.Ok = PostData(t.endpoint, "ps", file)
	return
}
