//go:build !linux
// +build !linux

package process

import (
	"context"
	"github.com/shirou/gopsutil/v3/internal/common"
)

func (p *Process) ThreadsWithName(ctx context.Context) (map[int32]Thread, error) {
	return nil, common.ErrNotImplementedError
}
