package capture

import (
	"context"
	"fmt"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/process"
	"io"
	"runtime"
	"shell/logger"
	"sort"
	"time"
)

type Process struct {
	Pid int32
	Cmd string
	MEM float32
	CPU float64
	p   *process.Process
}

func topCPU(n int, w io.Writer) (err error) {
	processes, err := process.Processes()
	if err != nil {
		return
	}
	result := make([]Process, 0, len(processes))
	for _, p := range processes {
		cmdline, err := p.Cmdline()
		if err != nil {
			logger.Debug().Err(err).Msg("failed to get cmdline")
			continue
		}
		if len(cmdline) == 0 {
			cmdline, err = p.Name()
			if err != nil {
				logger.Debug().Err(err).Msg("failed to get name")
				continue
			}
		}
		memoryPercent, err := p.MemoryPercent()
		if err != nil {
			logger.Debug().Err(err).Msg("failed to get memoryPercent")
			continue
		}
		_, err = p.Percent(0)
		if err != nil {
			logger.Debug().Err(err).Msg("failed to get cpuPercent")
			continue
		}
		result = append(result, Process{
			Pid: p.Pid,
			Cmd: cmdline,
			MEM: memoryPercent,
			p:   p,
		})
	}
	time.Sleep(time.Second)
	for i, p := range result {
		cpuPercent, err := p.p.Percent(0)
		if err != nil {
			logger.Debug().Err(err).Msg("failed to get cpuPercent")
			continue
		}
		result[i].CPU = cpuPercent
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CPU > result[j].CPU
	})
	if n > len(result) {
		n = len(result)
	}
	result = result[:n]
	_, err = fmt.Fprintln(w, "top:")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, "PID\t%CPU\t%MEM\tCOMMAND")
	if err != nil {
		return err
	}
	for _, p := range result {
		_, err = fmt.Fprintf(w, "%d\t%.2f\t%.2f\t%s\n", p.Pid, p.CPU, p.MEM, p.Cmd)
		if err != nil {
			return err
		}
	}
	return
}

type Thread struct {
	ID   int32
	CPU  float64
	Name string
}

func topHCPU(pid int, n int, w io.Writer) (err error) {
	target, err := process.NewProcess(int32(pid))
	if err != nil {
		return
	}
	threads1, err := target.ThreadsWithName(context.Background())
	if err != nil {
		return
	}
	time.Sleep(time.Second)
	threads2, err := target.ThreadsWithName(context.Background())
	if err != nil {
		return
	}
	result := make([]Thread, 0, len(threads2))
	numcpu := runtime.NumCPU()
	for id, thread2 := range threads2 {
		thread1, ok := threads1[id]
		if !ok {
			continue
		}
		delta := 1.0 * float64(numcpu)
		result = append(result, Thread{
			ID:   id,
			Name: thread2.Name,
			CPU:  calculatePercent(thread1.TimeStat, thread2.TimeStat, delta, numcpu),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].CPU > result[j].CPU
	})
	if n > len(result) {
		n = len(result)
	}
	result = result[:n]
	_, err = fmt.Fprintln(w, "toph:")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, "PID\t%CPU\t%MEM\tCOMMAND")
	if err != nil {
		return err
	}
	memoryPercent, err := target.MemoryPercent()
	if err != nil {
		return
	}
	for _, r := range result {
		_, err = fmt.Fprintf(w, "%d\t%.2f\t%.2f\t%s\n", r.ID, r.CPU, memoryPercent, r.Name)
		if err != nil {
			return err
		}
	}
	return
}

func calculatePercent(t1, t2 *cpu.TimesStat, delta float64, numcpu int) float64 {
	if delta == 0 {
		return 0
	}
	delta_proc := t2.Total() - t1.Total()
	overall_percent := ((delta_proc / delta) * 100) * float64(numcpu)
	return overall_percent
}
