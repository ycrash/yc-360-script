//go:build linux
// +build linux

package process

import (
	"context"
	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/internal/common"
	"golang.org/x/sys/unix"
	"io/ioutil"
	"strconv"
)

func (p *Process) ThreadsWithName(ctx context.Context) (map[int32]Thread, error) {
	ret := make(map[int32]Thread)
	taskPath := common.HostProc(strconv.Itoa(int(p.Pid)), "task")

	tids, err := readPidsFromDir(taskPath)
	if err != nil {
		return nil, err
	}

	for _, tid := range tids {
		fields, _, _, cpuTimes, _, _, _, _, err := p.FillFromTIDStatWithContext(ctx, tid)
		if err != nil {
			return nil, err
		}
		ret[tid] = Thread{
			Name:     fields[2],
			TimeStat: cpuTimes,
		}
	}

	return ret, nil
}

func (p *Process) FillFromTIDStatWithContext(ctx context.Context, tid int32) (fields []string, terminal uint64, id int32, cpuTimes *cpu.TimesStat, createTime int64, priority uint32, nice int32, faults *PageFaultsStat, err error) {
	pid := p.Pid
	var statPath string

	if tid == -1 {
		statPath = common.HostProc(strconv.Itoa(int(pid)), "stat")
	} else {
		statPath = common.HostProc(strconv.Itoa(int(pid)), "task", strconv.Itoa(int(tid)), "stat")
	}

	contents, err := ioutil.ReadFile(statPath)
	if err != nil {
		return
	}
	// Indexing from one, as described in `man proc` about the file /proc/[pid]/stat
	fields = splitProcStat(contents)

	terminal, err = strconv.ParseUint(fields[7], 10, 64)
	if err != nil {
		return
	}

	ppid, err := strconv.ParseInt(fields[4], 10, 32)
	if err != nil {
		return
	}
	utime, err := strconv.ParseFloat(fields[14], 64)
	if err != nil {
		return
	}

	stime, err := strconv.ParseFloat(fields[15], 64)
	if err != nil {
		return
	}

	// There is no such thing as iotime in stat file.  As an approximation, we
	// will use delayacct_blkio_ticks (aggregated block I/O delays, as per Linux
	// docs).  Note: I am assuming at least Linux 2.6.18
	var iotime float64
	if len(fields) > 42 {
		iotime, err = strconv.ParseFloat(fields[42], 64)
		if err != nil {
			iotime = 0 // Ancient linux version, most likely
		}
	} else {
		iotime = 0 // e.g. SmartOS containers
	}

	cpuTimes = &cpu.TimesStat{
		CPU:    "cpu",
		User:   utime / float64(clockTicks),
		System: stime / float64(clockTicks),
		Iowait: iotime / float64(clockTicks),
	}

	bootTime, _ := common.BootTimeWithContext(ctx)
	t, err := strconv.ParseUint(fields[22], 10, 64)
	if err != nil {
		return
	}
	ctime := (t / uint64(clockTicks)) + uint64(bootTime)
	createTime = int64(ctime * 1000)

	rtpriority, err := strconv.ParseInt(fields[18], 10, 32)
	if err != nil {
		return
	}
	if rtpriority < 0 {
		rtpriority = rtpriority*-1 - 1
	} else {
		rtpriority = 0
	}

	//	p.Nice = mustParseInt32(fields[18])
	// use syscall instead of parse Stat file
	snice, _ := unix.Getpriority(prioProcess, int(pid))
	nice = int32(snice) // FIXME: is this true?

	minFault, err := strconv.ParseUint(fields[10], 10, 64)
	if err != nil {
		return
	}
	cMinFault, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return
	}
	majFault, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return
	}
	cMajFault, err := strconv.ParseUint(fields[13], 10, 64)
	if err != nil {
		return
	}

	faults = &PageFaultsStat{
		MinorFaults:      minFault,
		MajorFaults:      majFault,
		ChildMinorFaults: cMinFault,
		ChildMajorFaults: cMajFault,
	}

	id = int32(ppid)
	priority = uint32(rtpriority)
	return
}
