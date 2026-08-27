package postgres

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"

	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/load"
	"github.com/shirou/gopsutil/v3/process"
)

// The two host-side reads. Disk describes the database's volumes and is taken only
// where this machine is confirmed to be the database's; the runner-health pair
// describes the machine the agent runs on and is taken only where it is not, so
// heartbeat numbers can be judged against the machine that measured them.

// tablespaceSQL lists the locations outside the data directory. Read every poll:
// a tablespace can be created while the server runs, and the list is tiny.
const tablespaceSQL = `SELECT pg_tablespace_location(oid)
FROM pg_catalog.pg_tablespace
WHERE pg_tablespace_location(oid) <> ''`

// readRunnerHealth takes the two numbers that say whether the runner itself was
// struggling. Skipped on a confirmed database host, where the top capture covers it.
func (p *PollResult) readRunnerHealth() {
	if p.OnDBHost() {
		return
	}

	// Windows has no load average; gopsutil emulates one, which would be a
	// different measurement under the same name.
	if runtime.GOOS != "windows" {
		if avg, err := load.Avg(); err == nil && avg != nil {
			p.RunnerLoad1 = strconv.FormatFloat(avg.Load1, 'f', 2, 64)
		}
	}

	p.AgentCPUPct = agentCPUPercent()
}

// agentCPUPercent measures over a short window rather than since process start,
// which on an agent running for weeks would report an average of nothing.
func agentCPUPercent() string {
	self, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return ""
	}

	pct, err := self.Percent(agentCPUInterval)
	if err != nil {
		return ""
	}

	return strconv.FormatFloat(pct, 'f', 1, 64)
}

// readDisk reports the fullest of the database's volumes. It runs only on a
// confirmed database host: the runner's own disk is never sent in its place.
func (p *PollResult) readDisk(ctx context.Context, q RowQuerier, dataDirectory, password string) {
	switch {
	case p.AgentOnDBHost == OnDBHostNo:
		p.DiskReason = diskReasonNotCoResident
		return

	case !p.OnDBHost():
		p.DiskReason = diskReasonDBHostUnknown
		return

	case dataDirectory == "":
		// The setting is readable by pg_monitor and NULL for a LOGIN-only role.
		p.DiskReason = diskReasonSettingUnavailable
		return
	}

	fullest, ok := p.fullestVolume(p.databasePaths(ctx, q, dataDirectory, password))
	if !ok {
		p.DiskReason = diskReasonReadFailed
		return
	}

	p.DiskFreeBytes = strconv.FormatUint(fullest.Free, 10)
	p.DiskTotalBytes = strconv.FormatUint(fullest.Total, 10)
	p.DiskMount = fullest.Path
}

// databasePaths is the data directory, the WAL directory beside it, and every
// tablespace. pg_wal is listed separately because it is often a link to its own
// volume, and a full WAL volume is the classic way a database stops.
func (p *PollResult) databasePaths(ctx context.Context, q RowQuerier, dataDirectory, password string) []string {
	paths := []string{dataDirectory, filepath.Join(dataDirectory, "pg_wal")}

	locations, err := readTablespaceLocations(ctx, q)
	if err != nil {
		// One missing grant costs the tablespaces, not the reading.
		p.Notes = append(p.Notes, "tablespace locations unread: "+errorText(err, password))

		return paths
	}

	return append(paths, locations...)
}

func readTablespaceLocations(ctx context.Context, q RowQuerier) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(ctx, tablespaceSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locations []string

	for rows.Next() {
		var location *string
		if err := rows.Scan(&location); err != nil {
			return nil, err
		}

		if location != nil && *location != "" {
			locations = append(locations, *location)
		}
	}

	return locations, rows.Err()
}

// volume is one path's answer, in bytes as a non-root user sees them.
type volume struct {
	Path  string
	Free  uint64
	Total uint64
}

// fullestVolume returns the least free of the readable paths, so one row describes
// the volume that will stop the database first. A path that cannot be read is
// skipped and logged; false means none could be.
func (p *PollResult) fullestVolume(paths []string) (volume, bool) {
	var (
		fullest volume
		found   bool
	)

	for _, path := range dedupePaths(paths) {
		usage, err := disk.Usage(path)
		if err != nil {
			p.Notes = append(p.Notes, "disk unread for "+path+": "+singleLine(err.Error()))

			continue
		}

		if !found || usage.Free < fullest.Free {
			fullest = volume{Path: path, Free: usage.Free, Total: usage.Total}
			found = true
		}
	}

	return fullest, found
}

// dedupePaths keeps the reading cheap and the answer stable: a cluster with no
// separate WAL volume would otherwise measure the data directory twice.
func dedupePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))

	var out []string

	for _, path := range paths {
		if path == "" {
			continue
		}

		if _, ok := seen[path]; ok {
			continue
		}

		seen[path] = struct{}{}
		out = append(out, path)
	}

	sort.Strings(out)

	return out
}
