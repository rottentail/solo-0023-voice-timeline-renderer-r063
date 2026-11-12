package checkflow

import "runtime"

func currentHeapSystem() uint64 {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return mem.Sys
}
