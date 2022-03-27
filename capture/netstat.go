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

func NewNetStat() *NetStat {
	n := &NetStat{}
	n.Add(1)
	return n
}

func (t *NetStat) Run() (result Result, err error) {
	file, err := os.Create("netstat.out")
	if err != nil {
		return
	}
	defer file.Close()
	file.WriteString(fmt.Sprintf("%s\n", shell.NowString()))
	err = shell.CommandCombinedOutputToWriter(file, shell.NetState)
	if err != nil {
		err = netStat(true, true, true, true, false, true, false, file)
		if err != nil {
			return
		}
	}
	t.Wait()
	file.WriteString(fmt.Sprintf("\n%s\n", shell.NowString()))
	err = shell.CommandCombinedOutputToWriter(file, shell.NetState)
	if err != nil {
		err = netStat(true, true, true, true, false, true, false, file)
		if err != nil {
			return
		}
	}
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "ns", file)
	return
}
