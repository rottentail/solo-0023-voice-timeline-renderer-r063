package checkflow

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// readProcessRSS returns the current resident set size in bytes. On Linux it
// reads /proc/self/statm (resident pages * page size); on other platforms it
// falls back to the Go runtime's system allocation estimate.
func readProcessRSS() uint64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return readRuntimeSystemBytes()
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return readRuntimeSystemBytes()
	}
	residentPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return readRuntimeSystemBytes()
	}
	pageSize := uint64(4096)
	if limit := uint64(syscall.Getpagesize()); limit > 0 {
		pageSize = limit
	}
	return residentPages * pageSize
}

func readRuntimeSystemBytes() uint64 {
	return currentHeapSystem()
}
