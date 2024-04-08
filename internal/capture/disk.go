package capture

import (
	"yc-agent/internal/capture/executils"
)

type Disk struct {
	Capture
}

func (t *Disk) Run() (result Result, err error) {
	df, err := executils.CommandCombinedOutputToFile("disk.out", executils.Disk)
	if err != nil {
		return
	}
	defer df.Close()
	result.Msg, result.Ok = PostData(t.endpoint, "df", df)
	return
}
