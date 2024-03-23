package capture

import (
	"shell/internal/utils"
)

type Disk struct {
	Capture
}

func (t *Disk) Run() (result Result, err error) {
	df, err := utils.CommandCombinedOutputToFile("disk.out", utils.Disk)
	if err != nil {
		return
	}
	defer df.Close()
	result.Msg, result.Ok = utils.PostData(t.endpoint, "df", df)
	return
}
