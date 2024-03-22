package capture

import "shell"

type Disk struct {
	Capture
}

func (t *Disk) Run() (result Result, err error) {
	df, err := shell.CommandCombinedOutputToFile("disk.out", shell.Disk)
	if err != nil {
		return
	}
	defer df.Close()
	result.Msg, result.Ok = shell.PostData(t.endpoint, "df", df)
	return
}
