// Package oracle implements an independent render verifier.
//
// The oracle deliberately shares no code with the production renderer,
// resampler, codec, or metrics packages. It only depends on the Go standard
// library and re-derives every expectation from the documented transform
// semantics and the raw source bytes, then compares three distinct planes:
//
//   - signal: dequantized output amplitudes versus an independently mixed
//     floating-point expectation;
//   - coordinates: exact integer frame geometry from source intervals through
//     rate conversion to receipt span placement;
//   - quantized_facts: the actual int16 sample values, saturation behaviour,
//     and digest/measurement facts after quantization.
package oracle

import "time"

// Plane identifies which independently checked fact family a violation belongs
// to. The planes are intentionally separate so a failure cannot be hidden by
// collapsing every disagreement into one loose numeric comparison.
type Plane string

const (
	PlaneSignal      Plane = "signal"
	PlaneCoordinates Plane = "coordinates"
	PlaneQuantized   Plane = "quantized_facts"
	PlaneReceipt     Plane = "receipt"
	PlaneSource      Plane = "source_facts"
	PlanePipeline    Plane = "pipeline"
)

// Source is one raw uploaded WAV plus the identifier used in the timeline.
type Source struct {
	ID  string
	WAV []byte
}

// Span is the timeline span contract, expressed without production types so
// the oracle never imports production packages.
type Span struct {
	SourceID         string
	SourceStartFrame int64
	SourceEndFrame   int64
	GainDB           float64
	CrossfadeFrames  int64
}

// Timeline is the submitted assembly contract.
type Timeline struct {
	ID             string
	SampleRate     int
	OutputChannels int
	TargetPeakDBFS float64
	Spans          []Span
}

// Candidate is one fully self-contained render experiment.
type Candidate struct {
	Sources  []Source
	Timeline Timeline
}

// SourceMetaFact is the source metadata the system-under-test reported back.
// Field tags mirror the public HTTP contract.
type SourceMetaFact struct {
	ID            string    `json:"id"`
	Digest        string    `json:"digest"`
	ByteCount     int64     `json:"byte_count"`
	SampleRate    int       `json:"sample_rate"`
	Channels      int       `json:"channels"`
	BitsPerSample int       `json:"bits_per_sample"`
	FrameCount    int64     `json:"frame_count"`
	DurationMS    int64     `json:"duration_ms"`
	PeakDBFS      float64   `json:"peak_dbfs"`
	RMSDBFS       float64   `json:"rms_dbfs"`
	CreatedAt     time.Time `json:"created_at"`
}

// ReceiptSpanFact mirrors one span entry of the public receipt contract.
type ReceiptSpanFact struct {
	Index            int     `json:"index"`
	SourceID         string  `json:"source_id"`
	SourceDigest     string  `json:"source_digest"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	OutputStartFrame int64   `json:"output_start_frame"`
	OutputEndFrame   int64   `json:"output_end_frame"`
	OutputFrames     int64   `json:"output_frames"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
	GainDB           float64 `json:"gain_db"`
	ChannelAction    string  `json:"channel_action"`
}

// NormalizationFact mirrors the normalization block of the receipt contract.
type NormalizationFact struct {
	RequestedPeakDBFS float64 `json:"requested_peak_dbfs"`
	BeforePeakDBFS    float64 `json:"before_peak_dbfs"`
	AppliedGainDB     float64 `json:"applied_gain_db"`
	OutputPeakDBFS    float64 `json:"output_peak_dbfs"`
}

// OutputFact mirrors the output block of the receipt contract.
type OutputFact struct {
	Digest        string  `json:"digest"`
	ByteCount     int64   `json:"byte_count"`
	SampleRate    int     `json:"sample_rate"`
	Channels      int     `json:"channels"`
	BitsPerSample int     `json:"bits_per_sample"`
	FrameCount    int64   `json:"frame_count"`
	DurationMS    int64   `json:"duration_ms"`
	PeakDBFS      float64 `json:"peak_dbfs"`
	RMSDBFS       float64 `json:"rms_dbfs"`
}

// ReceiptFact mirrors the public render receipt contract.
type ReceiptFact struct {
	SchemaVersion int               `json:"schema_version"`
	RenderID      string            `json:"render_id"`
	TimelineID    string            `json:"timeline_id"`
	Output        OutputFact        `json:"output"`
	Normalization NormalizationFact `json:"normalization"`
	Spans         []ReceiptSpanFact `json:"spans"`
	Warnings      []string          `json:"warnings"`
	CreatedAt     time.Time         `json:"created_at"`
}

// Violation is one concrete disagreement. Coordinates are exact where the fact
// is discrete; expected/actual carry rendered values for the signal plane.
type Violation struct {
	Plane    Plane  `json:"plane"`
	Code     string `json:"code"`
	Frame    int64  `json:"frame,omitempty"`
	Channel  int    `json:"channel,omitempty"`
	Span     int    `json:"span,omitempty"`
	Detail   string `json:"detail"`
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
}

// Report is the full verdict for one candidate.
type Report struct {
	Passed           bool        `json:"passed"`
	OutputFrames     int64       `json:"output_frames"`
	SampleRate       int         `json:"sample_rate"`
	Channels         int         `json:"channels"`
	AmbiguousSamples int         `json:"ambiguous_samples"`
	TotalSamples     int         `json:"total_samples"`
	Violations       []Violation `json:"violations"`
	// Observations records documented production-vs-spec convention
	// differences that do not fail the audit unless strict mode is enabled.
	Observations []Violation `json:"observations,omitempty"`
}

// Signature returns the stable identity of a failure used by the reducer to
// decide whether a shrunken candidate still replays the same signal violation.
func (v Violation) Signature() string {
	return string(v.Plane) + ":" + v.Code
}

// String renders the plane identifier.
func (p Plane) String() string { return string(p) }

// FirstViolation returns the first violation recorded, or false when the report
// is clean.
func (r Report) FirstViolation() (Violation, bool) {
	if len(r.Violations) == 0 {
		return Violation{}, false
	}
	return r.Violations[0], true
}
