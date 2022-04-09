package process

import "github.com/shirou/gopsutil/v3/cpu"

type Thread struct {
	Name     string
	TimeStat *cpu.TimesStat
}
