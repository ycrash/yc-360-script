package capture

import (
	"fmt"
	"os"
	"sync"

	"shell"
)

type NetStat struct {
	Capture
	sync.WaitGroup
}

func (t *NetStat) Run() (result Result, err error) {
	netstat, err := os.Create("netstat.out")
	if err != nil {
		return
	}
	defer netstat.Close()
	netstat.WriteString(fmt.Sprintf("%s\n", shell.NowString()))
	err = shell.CommandCombinedOutputToWriter(netstat, shell.NetState)
	if err != nil {
		return
	}
	t.Add(1)
	t.Wait()
	netstat.WriteString(fmt.Sprintf("\n%s\n", shell.NowString()))
	err = shell.CommandCombinedOutputToWriter(netstat, shell.NetState)
	if err != nil {
		return
	}
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "ns", netstat)
	return
}
