package capture

import (
	"fmt"
	"io"
	"os"
	"shell/internal"
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

	m := internal.SCRIPT_SPAN / internal.JAVACORE_INTERVAL
	for n := 1; n <= m; n++ {
		_, err = file.WriteString(fmt.Sprintf("\n%s\n", internal.NowString()))
		if err != nil {
			return
		}
		err = internal.CommandCombinedOutputToWriter(file, internal.PS)
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
			logger.Log("trying %v, cause %v exit code != 0", internal.PS2, internal.PS)
			err = internal.CommandCombinedOutputToWriter(file, internal.PS2)
			if err != nil {
				return
			}
		}
	}
	result.Msg, result.Ok = internal.PostData(t.endpoint, "ps", file)
	return
}
