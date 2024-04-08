package capture

import (
	"fmt"
	"os"
	"sync"

	"yc-agent/internal/capture/executils"
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
	file.WriteString(fmt.Sprintf("%s\n", executils.NowString()))
	err = executils.CommandCombinedOutputToWriter(file, executils.NetState)
	if err != nil {
		err = netStat(true, true, true, true, false, true, false, file)
		if err != nil {
			return
		}
	}
	t.Wait()
	file.WriteString(fmt.Sprintf("\n%s\n", executils.NowString()))
	err = executils.CommandCombinedOutputToWriter(file, executils.NetState)
	if err != nil {
		err = netStat(true, true, true, true, false, true, false, file)
		if err != nil {
			return
		}
	}
	result.Msg, result.Ok = PostData(t.Endpoint(), "ns", file)
	return
}
