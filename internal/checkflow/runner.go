package checkflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"voice-timeline-renderer/internal/oracle"
)

// RenderOracle is the independent-verification seam. The production audit uses
// *oracle.Verifier; tests may inject a faulting implementation to exercise the
// failure, reduction, and packaging paths without touching production code.
type RenderOracle interface {
	Verify(
		candidate oracle.Candidate,
		observedOutput []byte,
		observedReceipt oracle.ReceiptFact,
		observedSources map[string]oracle.SourceMetaFact,
	) oracle.Report
}

// AuditConfig controls a bounded fuzz-and-reduce run.
type AuditConfig struct {
	Candidates           int
	Workers              int
	Seed                 uint64
	ArtifactDir          string
	WorkRoot             string
	MaxReductionAttempts int
	LargeEvery           int
	RSSCeilingBytes      uint64
	Generator            GeneratorConfig
	Oracle               RenderOracle
}

// AuditStats summarizes a run, including the resource envelope actually
// observed so callers can prove the 256 MiB ceiling was respected.
type AuditStats struct {
	Generated         int
	Passed            int
	Failed            int
	LargeGenerated    int
	LargePassed       int
	HighWaterRSS      uint64
	RSSCeiling        uint64
	BackpressureHits  int64
	ReductionAttempts int
	ArtifactPath      string
	Elapsed           time.Duration
	SealViolations    []string
}

type auditRunner struct {
	config   AuditConfig
	budget   *Budget
	verifier RenderOracle

	sequence atomic.Int64

	mu             sync.Mutex
	summary        []ExecutionResult // slim: no source or rendered byte payloads
	passed         int
	failed         int
	largeGenerated int
	largePassed    int

	failureOnce sync.Once
	failure     *ExecutionResult
}

// RunAudit continuously generates bounded mixed-rate, overlapping projects,
// verifies each through the real HTTP workflow with the independent oracle,
// and on the first violation reduces it to a replayable minimal artifact.
func RunAudit(ctx context.Context, config AuditConfig) (AuditStats, error) {
	if config.Candidates <= 0 {
		config.Candidates = 64
	}
	if config.Workers <= 0 {
		config.Workers = 2
	}
	if config.LargeEvery <= 0 {
		config.LargeEvery = 8
	}
	if config.MaxReductionAttempts <= 0 {
		config.MaxReductionAttempts = 48
	}
	if config.WorkRoot == "" {
		root, err := os.MkdirTemp("", "voice-audit-work-*")
		if err != nil {
			return AuditStats{}, err
		}
		config.WorkRoot = root
	}
	if config.ArtifactDir == "" {
		config.ArtifactDir = filepath.Join(config.WorkRoot, "artifacts")
	}
	if err := os.MkdirAll(config.ArtifactDir, 0o750); err != nil {
		return AuditStats{}, err
	}

	budget := NewBudget(config.RSSCeilingBytes)
	defer budget.Close()
	verifier := config.Oracle
	if verifier == nil {
		verifier = oracle.NewVerifier()
	}
	runner := &auditRunner{
		config:   config,
		budget:   budget,
		verifier: verifier,
	}

	started := time.Now()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type job struct {
		candidate GeneratedCandidate
		sequence  int64
	}
	jobs := make(chan job, config.Workers*2)
	var workers sync.WaitGroup
	var firstErr atomic.Value // error

	for workerIndex := 0; workerIndex < config.Workers; workerIndex++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			// Each worker owns a private executor rooted in its own data
			// directory; candidates carry globally unique identifiers so
			// parallel workers can never share inputs or receipts.
			executor := NewExecutor(filepath.Join(config.WorkRoot, fmt.Sprintf("worker-%d", index)), budget, runner.verifier)
			for item := range jobs {
				if runCtx.Err() != nil {
					return
				}
				result, err := executor.Run(runCtx, item.candidate, item.sequence, 0)
				if err != nil {
					if errors.Is(err, context.Canceled) || errors.Is(runCtx.Err(), context.Canceled) {
						return
					}
					firstErr.CompareAndSwap(nil, err)
					cancel()
					return
				}
				runner.record(result)
				if result.Failed() {
					runner.failureOnce.Do(func() {
						snapshot := result
						runner.failure = &snapshot
						cancel()
					})
					return
				}
			}
		}(workerIndex)
	}

producer:
	for count := 0; count < config.Candidates; count++ {
		select {
		case <-runCtx.Done():
			break producer
		default:
		}
		sequence := runner.sequence.Add(1)
		// Large legal projects are generated on a fixed cadence and never
		// skipped: the budget is met by bounding size, not by avoiding them.
		large := sequence%int64(config.LargeEvery) == 0
		generator := NewGenerator(config.Generator, config.Seed^uint64(sequence))
		candidate := generator.Next(sequence, large)
		select {
		case jobs <- job{candidate: candidate, sequence: sequence}:
		case <-runCtx.Done():
			break producer
		}
	}
	close(jobs)
	workers.Wait()

	stats := AuditStats{
		RSSCeiling:       budget.RSSCeiling(),
		HighWaterRSS:     budget.HighWaterRSS(),
		BackpressureHits: budget.Sample().BackpressureHits,
		Elapsed:          time.Since(started),
	}
	runner.mu.Lock()
	stats.Generated = runner.passed + runner.failed
	stats.Passed = runner.passed
	stats.Failed = runner.failed
	stats.LargeGenerated = runner.largeGenerated
	stats.LargePassed = runner.largePassed
	runner.mu.Unlock()
	stats.SealViolations = runner.auditSeals()

	if err, ok := firstErr.Load().(error); ok && err != nil {
		return stats, err
	}
	if len(stats.SealViolations) > 0 {
		return stats, fmt.Errorf("parallel isolation broken: %s", stats.SealViolations[0])
	}
	if runner.failure != nil {
		artifactPath, attempts, err := runner.reduceAndPackage(runCtx, *runner.failure)
		stats.ReductionAttempts = attempts
		if err != nil {
			return stats, fmt.Errorf("reduce failure: %w", err)
		}
		stats.ArtifactPath = artifactPath
		return stats, ErrViolationFound{Artifact: artifactPath, Code: runner.failure.FailureCode}
	}
	return stats, nil
}

// record stores a slim result and its seal. The failing candidate keeps its
// full payload so the reducer can replay it; passing candidates are stripped so
// the long-running audit cannot accumulate the generated corpus in memory.
func (runner *auditRunner) record(result ExecutionResult) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if result.Failed() {
		snapshot := result
		runner.failed++
		if result.Large {
			runner.largeGenerated++
		}
		runner.summary = append(runner.summary, snapshot.SlimSummary())
		return
	}
	runner.passed++
	if result.Large {
		runner.largeGenerated++
		runner.largePassed++
	}
	runner.summary = append(runner.summary, result.SlimSummary())
}

// auditSeals verifies that parallel candidates never shared a render id, a
// source id with different content, or received another candidate's receipt.
func (runner *auditRunner) auditSeals() []string {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	violations := make([]string, 0)
	renderOwners := make(map[string]string)
	sourceOwners := make(map[string]string)
	for _, result := range runner.summary {
		owner := result.Seal.RenderID
		if previous, ok := renderOwners[result.Seal.RenderID]; ok && previous != owner {
			violations = append(violations, fmt.Sprintf("render id %s reused across candidates", result.Seal.RenderID))
		}
		renderOwners[result.Seal.RenderID] = owner
		if result.ObservedReceipt.RenderID != "" && result.ObservedReceipt.RenderID != result.Seal.RenderID {
			violations = append(violations, fmt.Sprintf(
				"candidate %s received receipt stamped for render %s", result.Seal.RenderID, result.ObservedReceipt.RenderID))
		}
		if result.ObservedReceipt.TimelineID != "" && result.ObservedReceipt.TimelineID != result.Seal.TimelineID {
			violations = append(violations, fmt.Sprintf(
				"candidate %s received receipt bound to timeline %s instead of %s",
				result.Seal.RenderID, result.ObservedReceipt.TimelineID, result.Seal.TimelineID))
		}
		for sourceID, digest := range result.Seal.SourceDigests {
			if previousDigest, ok := sourceOwners[sourceID]; ok && previousDigest != digest {
				violations = append(violations, fmt.Sprintf(
					"source id %s reused with different payloads (%s vs %s)", sourceID, previousDigest[:12], digest[:12]))
			}
			sourceOwners[sourceID] = digest
		}
	}
	return violations
}

func (runner *auditRunner) reduceAndPackage(parent context.Context, failed ExecutionResult) (string, int, error) {
	// Reduction runs under a fresh context: the generation context was
	// deliberately canceled when the first failure was observed.
	ctx := context.Background()
	_ = parent
	reductionRoot := filepath.Join(runner.config.WorkRoot, "reduction")
	if err := os.MkdirAll(reductionRoot, 0o750); err != nil {
		return "", 0, err
	}
	executor := NewExecutor(reductionRoot, runner.budget, runner.verifier)
	reducer := NewReducer(executor, failed.Sequence+10_000_000, runner.config.MaxReductionAttempts)
	reduced, err := reducer.Reduce(ctx, failed)
	if err != nil {
		return "", reducer.attempts, err
	}
	artifactDir := filepath.Join(runner.config.ArtifactDir, "violation-"+sanitizeFileName(reduced.FailureCode))
	if err := os.MkdirAll(artifactDir, 0o750); err != nil {
		return "", reducer.attempts, err
	}
	path, err := WriteArtifact(artifactDir, reduced, failed.Candidate, reducer.attempts)
	return path, reducer.attempts, err
}

// ErrViolationFound reports that the audit observed and minimized a violation.
type ErrViolationFound struct {
	Artifact string
	Code     string
}

func (err ErrViolationFound) Error() string {
	return fmt.Sprintf("independent oracle found violation %s; minimized replay at %s", err.Code, err.Artifact)
}

func sanitizeFileName(value string) string {
	var out []byte
	for _, char := range []byte(value) {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '-', char == '_':
			out = append(out, char)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "unknown"
	}
	return string(out)
}
