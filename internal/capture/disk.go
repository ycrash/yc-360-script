package capture

import (
	"shell/internal"
)

type Disk struct {
	Capture
}

func (t *Disk) Run() (result Result, err error) {
	df, err := internal.CommandCombinedOutputToFile("disk.out", internal.Disk)
	if err != nil {
		return
	}
	defer df.Close()
	result.Msg, result.Ok = internal.PostData(t.endpoint, "df", df)
	return
}
