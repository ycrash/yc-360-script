package capture

import (
	"fmt"
	"os"
	"shell"
)

type PS struct {
	Capture
}

func NewPS() *PS {
	p := &PS{}
	return p
}

func (t *PS) Run() (result Result, err error) {
	ps, err := os.Create("ps.out")
	if err != nil {
		return
	}
	defer ps.Close()

	m := shell.SCRIPT_SPAN / shell.JAVACORE_INTERVAL
	for n := 1; n <= m; n++ {
		_, err = ps.WriteString(fmt.Sprintf("\n%s\n", shell.NowString()))
		if err != nil {
			return
		}
		err = shell.CommandCombinedOutputToWriter(ps, shell.PS)
		if err != nil {
			return
		}
	}
	result.Msg, result.Ok = shell.PostData(t.endpoint, "ps", ps)
	return
}
