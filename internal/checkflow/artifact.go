package checkflow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/domain"
)

// ArtifactManifest is the self-contained replay description written next to
// the minimized sources and timeline.
type ArtifactManifest struct {
	SchemaVersion int                `json:"schema_version"`
	FailureCode   string             `json:"failure_code"`
	Violations    []violationRecord  `json:"violations"`
	Timeline      timelineRecord     `json:"timeline"`
	Sources       []sourceRecord     `json:"sources"`
	Seal          sealRecord         `json:"seal"`
	Replay        replayInstructions `json:"replay"`
	Reduction     reductionRecord    `json:"reduction"`
}

type violationRecord struct {
	Plane    string `json:"plane"`
	Code     string `json:"code"`
	Frame    int64  `json:"frame,omitempty"`
	Channel  int    `json:"channel,omitempty"`
	Span     int    `json:"span,omitempty"`
	Detail   string `json:"detail"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

type timelineRecord struct {
	ID             string                `json:"id"`
	SampleRate     int                   `json:"sample_rate"`
	OutputChannels int                   `json:"output_channels"`
	TargetPeakDBFS float64               `json:"target_peak_dbfs"`
	Spans          []domain.TimelineSpan `json:"spans"`
}

type sourceRecord struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Bytes  int    `json:"bytes"`
	Digest string `json:"digest"`
}

type sealRecord struct {
	RenderID      string            `json:"render_id"`
	TimelineID    string            `json:"timeline_id"`
	SourceIDs     []string          `json:"source_ids"`
	SourceDigests map[string]string `json:"source_digests"`
	OutputDigest  string            `json:"output_digest"`
}

type replayInstructions struct {
	Steps   []string `json:"steps"`
	Command string   `json:"command"`
}

type reductionRecord struct {
	OriginalSpans int `json:"original_spans"`
	ReducedSpans  int `json:"reduced_spans"`
	OriginalBytes int `json:"original_source_bytes"`
	ReducedBytes  int `json:"reduced_source_bytes"`
	Attempts      int `json:"attempts"`
}

// WriteArtifact packages a minimized failure into a self-contained directory:
// the exact minimal source WAVs, the reduced timeline contract, the violation
// report, isolation seals, and replay instructions. Nothing in the bundle
// references another candidate's data directory or receipt.
func WriteArtifact(
	dir string,
	result ExecutionResult,
	original GeneratedCandidate,
	attempts int,
) (string, error) {
	if err := os.MkdirAll(filepath.Join(dir, "sources"), 0o750); err != nil {
		return "", err
	}
	candidate := result.Candidate
	manifest := ArtifactManifest{
		SchemaVersion: 1,
		FailureCode:   result.FailureCode,
		Seal: sealRecord{
			RenderID:      result.Seal.RenderID,
			TimelineID:    result.Seal.TimelineID,
			SourceIDs:     append([]string(nil), result.Seal.SourceIDs...),
			SourceDigests: copyDigestMap(result.Seal.SourceDigests),
			OutputDigest:  result.Seal.OutputDigest,
		},
		Reduction: reductionRecord{
			OriginalSpans: len(original.Spans),
			ReducedSpans:  len(candidate.Spans),
			OriginalBytes: totalSourceBytes(original),
			ReducedBytes:  totalSourceBytes(candidate),
			Attempts:      attempts,
		},
	}
	for _, violation := range result.Report.Violations {
		manifest.Violations = append(manifest.Violations, violationRecord{
			Plane: string(violation.Plane), Code: violation.Code,
			Frame: violation.Frame, Channel: violation.Channel, Span: violation.Span,
			Detail: violation.Detail, Expected: violation.Expected, Actual: violation.Actual,
		})
	}
	manifest.Timeline = timelineRecord{
		ID: candidate.Oracle.Timeline.ID, SampleRate: candidate.SampleRate,
		OutputChannels: candidate.Channels, TargetPeakDBFS: candidate.TargetPeak,
		Spans: append([]domain.TimelineSpan(nil), candidate.Spans...),
	}
	for _, source := range candidate.Oracle.Sources {
		fileName := source.ID + ".wav"
		if err := os.WriteFile(filepath.Join(dir, "sources", fileName), source.WAV, 0o640); err != nil {
			return "", err
		}
		manifest.Sources = append(manifest.Sources, sourceRecord{
			ID: source.ID, File: "sources/" + fileName, Bytes: len(source.WAV),
			Digest: result.Seal.SourceDigests[source.ID],
		})
	}
	manifest.Replay = replayInstructions{
		Steps: []string{
			"Start a fresh voice-timeline service with an empty data directory.",
			"PUT each source in sources/ to /v1/sources/{id} using the exact WAV bytes.",
			"PUT the timeline from this manifest to /v1/timelines/{timeline_id}.",
			"PUT a render against that timeline, then fetch the receipt and output.",
			"Re-run the independent three-plane oracle; the same violation signature must appear.",
		},
		Command: "go run ./cmd/timelinecheck -workflow replay-artifact -artifact " + filepath.Base(dir) + "/manifest.json",
	}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, payload, 0o640); err != nil {
		return "", err
	}
	readme := fmt.Sprintf("# Reduced signal violation\n\nFailure: %s\nReduced spans: %d (from %d)\nReduced source bytes: %d (from %d)\n\nReplay:\n\n\t%s\n",
		result.FailureCode, manifest.Reduction.ReducedSpans, manifest.Reduction.OriginalSpans,
		manifest.Reduction.ReducedBytes, manifest.Reduction.OriginalBytes, manifest.Replay.Command)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o640); err != nil {
		return "", err
	}
	return manifestPath, nil
}

func totalSourceBytes(candidate GeneratedCandidate) int {
	total := 0
	for _, wav := range candidate.SourceWAVs {
		total += len(wav)
	}
	return total
}

func copyDigestMap(input map[string]string) map[string]string {
	out := make(map[string]string, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
