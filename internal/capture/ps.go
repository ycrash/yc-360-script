package capture

import (
	"fmt"
	"io"
	"os"

	"shell/internal/logger"
	"shell/internal/utils"
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

	m := utils.SCRIPT_SPAN / utils.JAVACORE_INTERVAL
	for n := 1; n <= m; n++ {
		_, err = file.WriteString(fmt.Sprintf("\n%s\n", utils.NowString()))
		if err != nil {
			return
		}
		err = utils.CommandCombinedOutputToWriter(file, utils.PS)
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
			logger.Log("trying %v, cause %v exit code != 0", utils.PS2, utils.PS)
			err = utils.CommandCombinedOutputToWriter(file, utils.PS2)
			if err != nil {
				return
			}
		}
	}
	result.Msg, result.Ok = utils.PostData(t.endpoint, "ps", file)
	return
}
