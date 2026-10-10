package apps

import (
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
)

// SizeLimit applies to both compressed uploads and expanded source. Zero uses
// the default; -1 lets the owner opt out of a byte limit.
type SizeLimit int64

func ParseSizeLimit(value string) (SizeLimit, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "unlimited" || value == "0" {
		return -1, nil
	}
	i := 0
	for i < len(value) && value[i] >= '0' && value[i] <= '9' {
		i++
	}
	n, err := strconv.ParseInt(value[:i], 10, 64)
	units := map[string]int64{"": 1, "b": 1, "kb": 1000, "mb": 1000_000, "gb": 1000_000_000, "tb": 1000_000_000_000, "kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40}
	unit, ok := units[strings.TrimSpace(value[i:])]
	if err != nil || !ok || n <= 0 || n > (math.MaxInt64-1)/unit {
		return 0, errors.New("app: --max-size must be a positive byte count, a size such as 512MiB or 2GiB, or unlimited")
	}
	return SizeLimit(n * unit), nil
}

func (l SizeLimit) Valid() bool {
	return l >= -1 && l < SizeLimit(math.MaxInt64)
}

func (l SizeLimit) Bytes() int64 {
	if l == 0 {
		return MaxArchive
	}
	return int64(l)
}

func (l SizeLimit) Exceeded(n int64) bool {
	return l.Bytes() > 0 && n > l.Bytes()
}

func (l SizeLimit) Allows(offset, additional int64) bool {
	return offset >= 0 && additional >= 0 && offset <= math.MaxInt64-additional && !l.Exceeded(offset+additional)
}

func (l SizeLimit) Reader(r io.Reader) io.Reader {
	if l.Bytes() < 0 {
		return r
	}
	return io.LimitReader(r, l.Bytes()+1)
}

// ArchiveExpansion allows tar framing and long-path records for each source file.
func (l SizeLimit) ArchiveExpansion() SizeLimit {
	if l.Bytes() < 0 {
		return l
	}
	return SizeLimit(min(l.Bytes(), math.MaxInt64-1-int64(maxSourceFiles*8192+1024)) + int64(maxSourceFiles*8192+1024))
}
