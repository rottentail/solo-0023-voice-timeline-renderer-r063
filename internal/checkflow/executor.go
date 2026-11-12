package checkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/oracle"
)

// Seal captures the identity facts that must never be shared between parallel
// candidates. The reducer and runner validate seals across every result to
// prove receipts and inputs were not cross-wired.
type Seal struct {
	Root          string
	RenderID      string
	TimelineID    string
	SourceIDs     []string
	SourceDigests map[string]string
	OutputDigest  string
	ReceiptRender string
}

// ExecutionResult is one verified candidate plus enough context to shrink or
// replay it. Byte payloads are carried only on the failure path; passing
// candidates return a slim summary so the long-running audit cannot accumulate
// the full generated corpus in memory.
type ExecutionResult struct {
	Sequence        int64
	Trial           int64
	Candidate       GeneratedCandidate
	Report          oracle.Report
	Seal            Seal
	ObservedReceipt oracle.ReceiptFact
	ObservedSources map[string]oracle.SourceMetaFact
	FailureCode     string
	Large           bool
}

// SlimSummary drops all byte payloads and rendered buffers, retaining only the
// facts the runner needs for stats and isolation audits.
func (result ExecutionResult) SlimSummary() ExecutionResult {
	slim := result
	slim.Candidate.SourceWAVs = nil
	slim.Candidate.Oracle.Sources = nil
	slim.ObservedSources = nil
	// Keep the seal and verdict but release the detailed violation buffers of
	// passing runs beyond the first few representative entries.
	if result.Report.Passed {
		slim.Report.Violations = nil
	}
	return slim
}

// Failed reports whether the independent verifier found a violation.
func (result ExecutionResult) Failed() bool { return !result.Report.Passed }

// Executor runs one candidate end to end through a private HTTP harness.
type Executor struct {
	parentRoot string
	budget     *Budget
	verifier   RenderOracle
}

func NewExecutor(parentRoot string, budget *Budget, verifier RenderOracle) *Executor {
	return &Executor{parentRoot: parentRoot, budget: budget, verifier: verifier}
}

// Run executes and independently verifies one candidate. On failure the
// harness data directory is retained under parentRoot for artifact packaging;
// on success it is removed before returning.
func (executor *Executor) Run(
	ctx context.Context,
	candidate GeneratedCandidate,
	sequence, trial int64,
) (ExecutionResult, error) {
	estimated := executor.estimateBytes(candidate)
	if err := executor.budget.Acquire(estimated); err != nil {
		return ExecutionResult{}, err
	}
	release := true
	defer func() {
		if release {
			executor.budget.Release()
		}
	}()

	root := fmt.Sprintf("%s/candidate-%d-%d", executor.parentRoot, sequence, trial)
	harness, err := NewIsolatedHarness(root)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("isolated harness: %w", err)
	}
	passed := false
	defer func() {
		if passed {
			harness.Close()
		} else {
			// Keep the exact replay surface for the failure artifact path.
			harness.Detach()
		}
	}()

	renderID := candidateRenderID(sequence, trial)
	observedSources := make(map[string]oracle.SourceMetaFact, len(candidate.Oracle.Sources))
	seal := Seal{
		Root:          root,
		RenderID:      renderID,
		TimelineID:    candidate.Oracle.Timeline.ID,
		SourceDigests: make(map[string]string, len(candidate.Oracle.Sources)),
	}

	for _, source := range candidate.Oracle.Sources {
		asset, status, err := harness.createSource(ctx, source.ID, "fuzz source "+source.ID, source.WAV)
		if err != nil {
			return ExecutionResult{}, fmt.Errorf("create source %s: %w", source.ID, err)
		}
		if status != 201 {
			return executor.reject(candidate, sequence, trial, "source_http_status_"+itoa(int64(status)), seal, observedSources, root)
		}
		observedSources[source.ID] = sourceMetaToOracle(asset)
		sum := sha256.Sum256(source.WAV)
		seal.SourceIDs = append(seal.SourceIDs, source.ID)
		seal.SourceDigests[source.ID] = hex.EncodeToString(sum[:])
	}

	plan, status, err := harness.createTimeline(
		ctx,
		candidate.Oracle.Timeline.ID,
		"fuzz timeline",
		candidate.SampleRate,
		candidate.Channels,
		candidate.TargetPeak,
		candidate.Spans,
	)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("create timeline: %w", err)
	}
	if status != 201 || plan.State != "ready" {
		return executor.reject(candidate, sequence, trial, "timeline_http_status_"+itoa(int64(status)), seal, observedSources, root)
	}

	job, status, err := harness.createRender(ctx, renderID, plan.ID)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("create render: %w", err)
	}
	if status != 201 {
		return executor.reject(candidate, sequence, trial, "render_http_status_"+itoa(int64(status)), seal, observedSources, root)
	}
	if job.ID != renderID || job.TimelineID != plan.ID {
		return executor.reject(candidate, sequence, trial, "render_identity_leak", seal, observedSources, root)
	}
	if job.State != domain.RenderCompleted || job.Output == nil {
		code := "render_not_completed"
		if job.Error != nil {
			code = job.Error.Code
		}
		return executor.reject(candidate, sequence, trial, code, seal, observedSources, root)
	}

	receipt, status, err := harness.getReceipt(ctx, renderID)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("read receipt: %w", err)
	}
	if status != 200 {
		return executor.reject(candidate, sequence, trial, "receipt_http_status_"+itoa(int64(status)), seal, observedSources, root)
	}
	output, status, err := harness.getOutput(ctx, renderID)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("read output: %w", err)
	}
	if status != 200 {
		return executor.reject(candidate, sequence, trial, "output_http_status_"+itoa(int64(status)), seal, observedSources, root)
	}

	receiptFact := receiptToOracle(receipt)
	seal.ReceiptRender = receiptFact.RenderID
	seal.OutputDigest = receiptFact.Output.Digest
	if receiptFact.RenderID != renderID {
		return executor.reject(candidate, sequence, trial, "receipt_render_id_crosswire", seal, observedSources, root)
	}
	sum := sha256.Sum256(output)
	if hex.EncodeToString(sum[:]) != receiptFact.Output.Digest {
		return executor.reject(candidate, sequence, trial, "output_digest_tampered", seal, observedSources, root)
	}

	report := executor.verifier.Verify(candidate.Oracle, output, receiptFact, observedSources)
	result := ExecutionResult{
		Sequence:        sequence,
		Trial:           trial,
		Candidate:       candidate,
		Report:          report,
		Seal:            seal,
		ObservedReceipt: receiptFact,
		ObservedSources: observedSources,
		Large:           candidate.Large,
	}
	if report.Passed {
		passed = true
		release = false
		executor.budget.Release()
		return result, nil
	}
	if first, ok := report.FirstViolation(); ok {
		result.FailureCode = string(first.Plane) + ":" + first.Code
	}
	return result, nil
}

func (executor *Executor) reject(
	candidate GeneratedCandidate,
	sequence, trial int64,
	code string,
	seal Seal,
	sources map[string]oracle.SourceMetaFact,
	root string,
) (ExecutionResult, error) {
	return ExecutionResult{
		Sequence:        sequence,
		Trial:           trial,
		Candidate:       candidate,
		FailureCode:     string(oracle.PlanePipeline) + ":" + code,
		Seal:            seal,
		ObservedSources: sources,
		Report: oracle.Report{
			Passed:     false,
			Violations: []oracle.Violation{{Plane: oracle.PlanePipeline, Code: code, Detail: "HTTP workflow rejected candidate at " + root}},
		},
		Large: candidate.Large,
	}, nil
}

func (executor *Executor) estimateBytes(candidate GeneratedCandidate) int {
	total := 0
	for _, source := range candidate.Oracle.Sources {
		// base64 request body, stored bytes, decoded float64/float32 working
		// copies, and resample/map intermediates.
		total += len(source.WAV) * 6
	}
	// Two output-sized float buffers, the encoded WAV, and JSON envelopes.
	total += int(candidate.OutputFrames)*candidate.Channels*4*2 +
		int(candidate.OutputFrames)*candidate.Channels*2 + (1 << 20)
	return total
}

func sourceMetaToOracle(asset domain.SourceAsset) oracle.SourceMetaFact {
	return oracle.SourceMetaFact{
		ID:            asset.ID,
		Digest:        asset.Digest,
		ByteCount:     asset.ByteCount,
		SampleRate:    asset.SampleRate,
		Channels:      asset.Channels,
		BitsPerSample: asset.BitsPerSample,
		FrameCount:    asset.FrameCount,
		DurationMS:    asset.DurationMS,
		PeakDBFS:      asset.PeakDBFS,
		RMSDBFS:       asset.RMSDBFS,
		CreatedAt:     asset.CreatedAt,
	}
}

func receiptToOracle(receipt domain.RenderReceipt) oracle.ReceiptFact {
	spans := make([]oracle.ReceiptSpanFact, 0, len(receipt.Spans))
	for _, span := range receipt.Spans {
		spans = append(spans, oracle.ReceiptSpanFact{
			Index:            span.Index,
			SourceID:         span.SourceID,
			SourceDigest:     span.SourceDigest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: span.OutputStartFrame,
			OutputEndFrame:   span.OutputEndFrame,
			OutputFrames:     span.OutputFrames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    span.ChannelAction,
		})
	}
	return oracle.ReceiptFact{
		SchemaVersion: receipt.SchemaVersion,
		RenderID:      receipt.RenderID,
		TimelineID:    receipt.TimelineID,
		Output: oracle.OutputFact{
			Digest:        receipt.Output.Digest,
			ByteCount:     receipt.Output.ByteCount,
			SampleRate:    receipt.Output.SampleRate,
			Channels:      receipt.Output.Channels,
			BitsPerSample: receipt.Output.BitsPerSample,
			FrameCount:    receipt.Output.FrameCount,
			DurationMS:    receipt.Output.DurationMS,
			PeakDBFS:      receipt.Output.PeakDBFS,
			RMSDBFS:       receipt.Output.RMSDBFS,
		},
		Normalization: oracle.NormalizationFact{
			RequestedPeakDBFS: receipt.Normalization.RequestedPeakDBFS,
			BeforePeakDBFS:    receipt.Normalization.BeforePeakDBFS,
			AppliedGainDB:     receipt.Normalization.AppliedGainDB,
			OutputPeakDBFS:    receipt.Normalization.OutputPeakDBFS,
		},
		Spans:     spans,
		Warnings:  append([]string(nil), receipt.Warnings...),
		CreatedAt: receipt.CreatedAt,
	}
}
