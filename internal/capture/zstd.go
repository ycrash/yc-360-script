package capture

import (
	"io"

	"github.com/klauspost/compress/zstd"
)

const (
	zstdLevel              = 1
	zstdEncoderConcurrency = 1
)

func newZstdEncoder(w io.Writer) (*zstd.Encoder, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(zstdLevel)),
		zstd.WithEncoderConcurrency(zstdEncoderConcurrency),
	)
}
