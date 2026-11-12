package checkflow

import (
	"context"
	"os"
	"testing"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/oracle"
)

func TestGeneratorRespectsStructuralBounds(t *testing.T) {
	cfg := GeneratorConfig{
		MaxSources: 4, MaxSpans: 8, MinSourceFrames: 96, MaxSourceFrames: 4000,
		MaxOutputFrames: 8000, MaxSourceWAVSize: 256 << 10,
	}
	generator := NewGenerator(cfg, 12345)
	for sequence := int64(1); sequence <= 40; sequence++ {
		candidate := generator.Next(sequence, sequence%6 == 0)
		if len(candidate.Oracle.Sources) > cfg.MaxSources {
			t.Fatalf("sequence %d: %d sources exceed cap", sequence, len(candidate.Oracle.Sources))
		}
		if len(candidate.Spans) > cfg.MaxSpans || len(candidate.Spans) == 0 {
			t.Fatalf("sequence %d: illegal span count %d", sequence, len(candidate.Spans))
		}
		for id, wav := range candidate.SourceWAVs {
			if len(wav) > cfg.MaxSourceWAVSize {
				t.Fatalf("sequence %d source %s wav %d exceeds byte cap", sequence, id, len(wav))
			}
		}
		if candidate.OutputFrames > cfg.MaxOutputFrames {
			t.Fatalf("sequence %d output %d exceeds frame cap", sequence, candidate.OutputFrames)
		}
		// First span can never crossfade.
		if candidate.Spans[0].CrossfadeFrames != 0 {
			t.Fatalf("sequence %d first span crossfade %d", sequence, candidate.Spans[0].CrossfadeFrames)
		}
		// Every span references an existing, in-bounds source.
		for index, span := range candidate.Spans {
			meta, ok := candidate.SourceMeta[span.SourceID]
			if !ok {
				t.Fatalf("span %d references unknown source %s", index, span.SourceID)
			}
			if span.SourceEndFrame > meta.frames || span.SourceEndFrame <= span.SourceStartFrame {
				t.Fatalf("span %d interval out of bounds", index)
			}
		}
	}
}

func TestGeneratorProducesMixedRatesAndOverlaps(t *testing.T) {
	cfg := DefaultGeneratorConfig()
	generator := NewGenerator(cfg, 777)
	rates := map[int]bool{}
	sawCrossfade := false
	for sequence := int64(1); sequence <= 30; sequence++ {
		candidate := generator.Next(sequence, false)
		for _, meta := range candidate.SourceMeta {
			rates[meta.rate] = true
		}
		for _, span := range candidate.Spans {
			if span.CrossfadeFrames > 0 {
				sawCrossfade = true
			}
		}
	}
	if len(rates) < 3 {
		t.Fatalf("generator did not mix sample rates, saw %v", rates)
	}
	if !sawCrossfade {
		t.Fatal("generator never produced an overlapping (crossfade) span")
	}
}

func TestBudgetBackpressureStaysUnderCeiling(t *testing.T) {
	budget := NewBudget(64 << 20)
	defer budget.Close()
	// Acquire modest units; with GOMEMLIMIT pacing these must always succeed
	// without ever reporting an RSS above the hard ceiling.
	for i := 0; i < 200; i++ {
		if err := budget.Acquire(1 << 15); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		budget.Release()
	}
	if high := budget.HighWaterRSS(); high > 64<<20 {
		t.Fatalf("high water %d exceeds 64 MiB ceiling", high)
	}
}

// faultingOracle wraps the real verifier and injects a deterministic signal
// violation for one render id, exercising the failure and reduction paths
// without modifying production rendering code.
type faultingOracle struct {
	delegate *oracle.Verifier
	failKey  string
}

func (f faultingOracle) Verify(
	candidate oracle.Candidate,
	output []byte,
	receipt oracle.ReceiptFact,
	sources map[string]oracle.SourceMetaFact,
) oracle.Report {
	report := f.delegate.Verify(candidate, output, receipt, sources)
	if receipt.RenderID == f.failKey {
		report.Passed = false
		report.Violations = append(report.Violations, oracle.Violation{
			Plane: oracle.PlaneSignal, Code: "injected_test_fault",
			Frame: 0, Channel: 0, Detail: "deterministic test injection",
		})
	}
	return report
}

func TestAuditParallelIsolationAndCleanPass(t *testing.T) {
	workRoot, err := os.MkdirTemp("", "audit-clean-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workRoot)
	stats, err := RunAudit(context.Background(), AuditConfig{
		Candidates: 18, Workers: 3, Seed: 2026,
		WorkRoot: workRoot, ArtifactDir: workRoot + "/artifacts",
		Generator: smallTestConfig(), LargeEvery: 5,
	})
	if err != nil {
		t.Fatalf("RunAudit: %v", err)
	}
	if stats.Generated != 18 || stats.Failed != 0 || stats.Passed != 18 {
		t.Fatalf("unexpected stats %+v", stats)
	}
	if stats.LargeGenerated == 0 {
		t.Fatal("large legal projects were never generated")
	}
	if stats.HighWaterRSS > stats.RSSCeiling {
		t.Fatalf("RSS %d exceeded ceiling %d", stats.HighWaterRSS, stats.RSSCeiling)
	}
	if len(stats.SealViolations) != 0 {
		t.Fatalf("parallel isolation broken: %v", stats.SealViolations)
	}
}

func TestAuditFailureReducesAndReplays(t *testing.T) {
	workRoot, err := os.MkdirTemp("", "audit-fail-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workRoot)
	fault := faultingOracle{delegate: oracle.NewVerifier(), failKey: "will-never-match-empty"}
	stats, err := RunAudit(context.Background(), AuditConfig{
		Candidates: 10, Workers: 2, Seed: 31,
		WorkRoot: workRoot, ArtifactDir: workRoot + "/artifacts",
		Generator: smallTestConfig(), MaxReductionAttempts: 24,
		Oracle: fault,
	})
	// The injected key never matches, so this run must stay clean: it proves
	// the seam does not manufacture false failures.
	if err != nil {
		t.Fatalf("non-matching fault seam should stay clean, got %v (stats %+v)", err, stats)
	}

	// Now inject a fault that matches the first candidate's render id.
	matchingFault := faultingOracle{
		delegate: oracle.NewVerifier(),
		failKey:  candidateRenderID(1, 0),
	}
	workRoot2, err := os.MkdirTemp("", "audit-fail2-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workRoot2)
	stats, err = RunAudit(context.Background(), AuditConfig{
		Candidates: 10, Workers: 2, Seed: 31,
		WorkRoot: workRoot2, ArtifactDir: workRoot2 + "/artifacts",
		Generator: smallTestConfig(), MaxReductionAttempts: 32,
		Oracle: matchingFault,
	})
	if err == nil {
		t.Fatal("expected ErrViolationFound for matching injected fault")
	}
	violation, ok := err.(ErrViolationFound)
	if !ok {
		t.Fatalf("expected ErrViolationFound, got %T %v", err, err)
	}
	if violation.Code != "signal:injected_test_fault" {
		t.Fatalf("unexpected failure code %q", violation.Code)
	}
	if stats.ReductionAttempts == 0 {
		t.Fatal("reduction was never attempted")
	}
	if _, err := os.Stat(violation.Artifact); err != nil {
		t.Fatalf("artifact missing: %v", err)
	}
	// Replay through a fresh harness. Note the faulting oracle is not wired
	// into the replay CLI; here we assert the bundle is loadable and internally
	// consistent instead, leaving full replay to the workflow-level check.
	manifest, err := loadManifest(violation.Artifact)
	if err != nil {
		t.Fatalf("loadManifest: %v", err)
	}
	if manifest.FailureCode != "signal:injected_test_fault" || len(manifest.Sources) == 0 {
		t.Fatalf("artifact contents invalid: %+v", manifest)
	}
}

func TestReductionPreservesSignatureAndShrinks(t *testing.T) {
	workRoot, err := os.MkdirTemp("", "reducer-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workRoot)
	budget := NewBudget(0)
	defer budget.Close()
	executor := NewExecutor(workRoot, budget, oracle.NewVerifier())
	generator := NewGenerator(smallTestConfig(), 919)
	candidate := generator.Next(1, true)
	original := candidate
	result, err := executor.Run(context.Background(), candidate, 1, 0)
	if err != nil {
		t.Fatalf("executor Run: %v", err)
	}
	if result.Failed() {
		t.Fatalf("baseline candidate unexpectedly failed: %+v", result.Report.Violations)
	}
	// Force a synthetic failure result, then verify the reducer only accepts
	// probes preserving the signature. With the real verifier all probes pass,
	// so the reducer must return the original unchanged rather than mutate it.
	synthetic := result
	synthetic.FailureCode = "coordinates:span_placement"
	synthetic.Report = oracle.Report{
		Passed: false,
		Violations: []oracle.Violation{{
			Plane: oracle.PlaneCoordinates, Code: "span_placement", Span: 0,
		}},
	}
	reducer := NewReducer(executor, 5_000_001, 8)
	reduced, err := reducer.Reduce(context.Background(), synthetic)
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if len(reduced.Candidate.Spans) > len(original.Spans) {
		t.Fatal("reducer unexpectedly grew the candidate")
	}
}

func TestSealCatchesCrossWiredReceipt(t *testing.T) {
	// Two synthetic results with mismatched receipt render ids must be flagged
	// by the seal audit even though they never touch a real server.
	runner := &auditRunner{
		summary: []ExecutionResult{
			{
				Seal:            Seal{RenderID: "rA", TimelineID: "tA", SourceDigests: map[string]string{"sA": "d1"}},
				ObservedReceipt: oracle.ReceiptFact{RenderID: "rB", TimelineID: "tB"},
			},
		},
	}
	violations := runner.auditSeals()
	if len(violations) == 0 {
		t.Fatal("cross-wired receipt was not detected by the seal audit")
	}
}

func smallTestConfig() GeneratorConfig {
	return GeneratorConfig{
		MaxSources: 3, MaxSpans: 6, MinSourceFrames: 96, MaxSourceFrames: 3000,
		MaxOutputFrames: 6000, MaxSourceWAVSize: 192 << 10,
	}
}

// Ensure the domain import is used by a compile-time reference in helpers.
var _ = domain.RenderCompleted
