package checkflow

import (
	"errors"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

// Budget is the hard process resource envelope. The RSS ceiling is 256 MiB;
// GOMEMLIMIT is set below it so the Go runtime returns memory to the OS before
// the ceiling can be reached. The guardian never relaxes tolerances, drops
// candidates, or stops generating large legal projects. Instead it applies
// back-pressure: workers wait for memory to drain, and completed artifacts are
// released deterministically.
type Budget struct {
	rssCeilingBytes   uint64
	softTriggerBytes  uint64
	pollInterval      time.Duration
	gcPercent         int
	highWaterRSS      atomic.Uint64
	backpressureWaits atomic.Int64
	stop              chan struct{}
	stopOnce          sync.Once
}

const (
	defaultRSSCeiling      = 256 << 20
	defaultSoftTriggerFrac = 0.78
)

// NewBudget installs a soft memory limit and GC pacing under the requested RSS
// ceiling and returns the guardian. Call Close to restore prior runtime
// settings.
func NewBudget(ceiling uint64) *Budget {
	if ceiling == 0 {
		ceiling = defaultRSSCeiling
	}
	budget := &Budget{
		rssCeilingBytes:  ceiling,
		softTriggerBytes: uint64(float64(ceiling) * defaultSoftTriggerFrac),
		pollInterval:     5 * time.Millisecond,
		gcPercent:        50,
		stop:             make(chan struct{}),
	}
	debug.SetMemoryLimit(int64(float64(ceiling) * 0.72))
	debug.SetGCPercent(budget.gcPercent)
	return budget
}

// Close restores runtime defaults and unblocks any waiting acquisition.
func (budget *Budget) Close() {
	budget.stopOnce.Do(func() { close(budget.stop) })
	debug.SetMemoryLimit(-1)
	debug.SetGCPercent(100)
}

// RSSCeiling reports the hard ceiling in bytes.
func (budget *Budget) RSSCeiling() uint64 { return budget.rssCeilingBytes }

// Stats is a point-in-time resource snapshot.
type Stats struct {
	AllocBytes       uint64
	TotalAllocBytes  uint64
	SystemBytes      uint64
	ResidentSetBytes uint64
	NumGC            uint32
	BackpressureHits int64
}

// Sample reads runtime memory stats and an OS-level RSS estimate.
func (budget *Budget) Sample() Stats {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	return Stats{
		AllocBytes:       mem.Alloc,
		TotalAllocBytes:  mem.TotalAlloc,
		SystemBytes:      mem.Sys,
		ResidentSetBytes: readProcessRSS(),
		NumGC:            mem.NumGC,
		BackpressureHits: budget.backpressureWaits.Load(),
	}
}

// Acquire blocks until there is headroom for an allocation of the requested
// size, forcing garbage collection while pressure remains. It returns
// ErrBudgetClosed if the guardian was shut down.
func (budget *Budget) Acquire(size int) error {
	for {
		select {
		case <-budget.stop:
			return ErrBudgetClosed
		default:
		}

		stats := budget.Sample()
		budget.observeHighWater(stats.ResidentSetBytes)
		projectedRSS := stats.ResidentSetBytes + uint64(size)
		projectedHeap := stats.AllocBytes + uint64(size)
		if projectedRSS <= budget.softTriggerBytes && projectedHeap <= budget.softTriggerBytes {
			return nil
		}
		runtime.GC()
		stats = budget.Sample()
		budget.observeHighWater(stats.ResidentSetBytes)
		projectedRSS = stats.ResidentSetBytes + uint64(size)
		projectedHeap = stats.AllocBytes + uint64(size)
		if projectedRSS <= budget.softTriggerBytes && projectedHeap <= budget.softTriggerBytes {
			return nil
		}
		budget.backpressureWaits.Add(1)
		select {
		case <-budget.stop:
			return ErrBudgetClosed
		case <-time.After(budget.pollInterval * 4):
		}
	}
}

// Release records that an allocation unit has been freed. GC pacing and the
// poll loop re-evaluate headroom; this hook exists so callers keep the acquire
// and release lifecycle explicit.
func (budget *Budget) Release() {
	runtime.GC()
}

// Abort unblocks all waiters during shutdown.
func (budget *Budget) Abort() {
	budget.stopOnce.Do(func() { close(budget.stop) })
}

// HighWaterRSS returns the largest RSS sample observed so far.
func (budget *Budget) HighWaterRSS() uint64 { return budget.highWaterRSS.Load() }

func (budget *Budget) observeHighWater(value uint64) {
	for {
		current := budget.highWaterRSS.Load()
		if value <= current || budget.highWaterRSS.CompareAndSwap(current, value) {
			return
		}
	}
}

// ErrBudgetClosed indicates the guardian stopped before an acquisition.
var ErrBudgetClosed = errors.New("resource budget guardian is closed")
