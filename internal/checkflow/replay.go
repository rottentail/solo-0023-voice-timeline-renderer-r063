package checkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/oracle"
)

// ReplayArtifact executes a previously minimized failure bundle against a
// fresh HTTP harness and asserts the same violation signature reappears. It is
// the concrete proof that the reduced sources and timeline replay the original
// signal, coordinate, or quantization violation without any leftover process
// state.
func ReplayArtifact(ctx context.Context, manifestPath string) (oracle.Report, error) {
	manifest, err := loadManifest(manifestPath)
	if err != nil {
		return oracle.Report{}, err
	}
	baseDir := filepath.Dir(manifestPath)

	tempDir, err := os.MkdirTemp("", "voice-replay-*")
	if err != nil {
		return oracle.Report{}, err
	}
	harness, err := NewIsolatedHarness(tempDir)
	if err != nil {
		_ = os.RemoveAll(tempDir)
		return oracle.Report{}, err
	}
	defer harness.Close()

	candidate := oracle.Candidate{
		Timeline: oracle.Timeline{
			ID:             manifest.Timeline.ID,
			SampleRate:     manifest.Timeline.SampleRate,
			OutputChannels: manifest.Timeline.OutputChannels,
			TargetPeakDBFS: manifest.Timeline.TargetPeakDBFS,
		},
	}
	observedSources := make(map[string]oracle.SourceMetaFact, len(manifest.Sources))
	for _, record := range manifest.Sources {
		wavPath := filepath.Join(baseDir, record.File)
		wav, err := os.ReadFile(wavPath)
		if err != nil {
			return oracle.Report{}, fmt.Errorf("read replay source %s: %w", record.ID, err)
		}
		sum := sha256.Sum256(wav)
		digest := hex.EncodeToString(sum[:])
		if record.Digest != "" && record.Digest != digest {
			return oracle.Report{}, fmt.Errorf("replay source %s digest %s does not match manifest %s", record.ID, digest, record.Digest)
		}
		candidate.Sources = append(candidate.Sources, oracle.Source{ID: record.ID, WAV: wav})
		asset, status, err := harness.createSource(ctx, record.ID, "replay source "+record.ID, wav)
		if err != nil {
			return oracle.Report{}, err
		}
		if status != 201 {
			return oracle.Report{}, fmt.Errorf("replay source %s returned status %d", record.ID, status)
		}
		observedSources[record.ID] = sourceMetaToOracle(asset)
	}
	for _, span := range manifest.Timeline.Spans {
		candidate.Timeline.Spans = append(candidate.Timeline.Spans, oracle.Span{
			SourceID:         span.SourceID,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			GainDB:           span.GainDB,
			CrossfadeFrames:  span.CrossfadeFrames,
		})
	}

	plan, status, err := harness.createTimeline(
		ctx,
		manifest.Timeline.ID,
		"replay timeline",
		manifest.Timeline.SampleRate,
		manifest.Timeline.OutputChannels,
		manifest.Timeline.TargetPeakDBFS,
		manifest.Timeline.Spans,
	)
	if err != nil {
		return oracle.Report{}, err
	}
	if status != 201 {
		return oracle.Report{}, fmt.Errorf("replay timeline returned status %d", status)
	}
	renderID := "replay_" + manifest.Seal.RenderID
	job, status, err := harness.createRender(ctx, renderID, plan.ID)
	if err != nil {
		return oracle.Report{}, err
	}
	if status != 201 || job.State != "completed" {
		return oracle.Report{}, fmt.Errorf("replay render returned status=%d state=%s", status, job.State)
	}
	receipt, status, err := harness.getReceipt(ctx, renderID)
	if err != nil {
		return oracle.Report{}, err
	}
	if status != 200 {
		return oracle.Report{}, fmt.Errorf("replay receipt returned status %d", status)
	}
	output, status, err := harness.getOutput(ctx, renderID)
	if err != nil {
		return oracle.Report{}, err
	}
	if status != 200 {
		return oracle.Report{}, fmt.Errorf("replay output returned status %d", status)
	}

	report := oracle.NewVerifier().Verify(candidate, output, receiptToOracle(receipt), observedSources)
	if report.Passed {
		return report, fmt.Errorf("replay did not reproduce the recorded violation %s", manifest.FailureCode)
	}
	signature := ""
	if first, ok := report.FirstViolation(); ok {
		signature = first.Signature()
	}
	if signature != manifest.FailureCode {
		return report, fmt.Errorf("replay produced %s but the artifact records %s", signature, manifest.FailureCode)
	}
	return report, nil
}

func loadManifest(path string) (ArtifactManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ArtifactManifest{}, err
	}
	var manifest ArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ArtifactManifest{}, err
	}
	if manifest.SchemaVersion != 1 {
		return ArtifactManifest{}, fmt.Errorf("unsupported artifact schema version %d", manifest.SchemaVersion)
	}
	return manifest, nil
}
