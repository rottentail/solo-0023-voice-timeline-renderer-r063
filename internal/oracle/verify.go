package oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
)

// Verifier runs fully independent render verification. It never imports a
// production package: every expectation is recomputed here from raw bytes and
// the submitted contract.
type Verifier struct {
	// SignalTolerance bounds the dequantized-amplitude comparison. It is
	// derived from the float32 accumulation envelope plus one quantization bin,
	// not chosen to make tests pass.
	SignalTolerance float64
	// DBTolerance bounds decibel fact comparisons where float32/float64
	// ordering differences can move the last decimal place.
	DBTolerance float64
	// StrictConventions makes documented production-vs-spec convention
	// differences fail the audit instead of being recorded as observations.
	// The three core planes (signal, coordinates, quantized facts) are always
	// strict regardless of this switch.
	StrictConventions bool
}

// NewVerifier returns a verifier with principled fixed tolerances.
func NewVerifier() *Verifier {
	return &Verifier{
		// one PCM16 bin (~3.05e-5) plus the float32 accumulation envelope.
		SignalTolerance: 1.0/pcm16MaxPositive + 1.5e-6,
		DBTolerance:     1e-3,
	}
}

// conventionViolation either fails the report (strict mode) or records an
// observation on that report, while always leaving an audit trail. The
// verifier itself holds no per-call mutable state, so it is safe to share.
func (v *Verifier) conventionViolation(report *Report, violation Violation) {
	if v.StrictConventions {
		report.Passed = false
		report.Violations = append(report.Violations, violation)
		return
	}
	report.Observations = append(report.Observations, violation)
}

// Verify independently checks one completed render. observedOutput is the WAV
// bytes downloaded through the real HTTP interface, observedReceipt is the
// receipt resource, and observedSources are the metadata returned by the
// source registration calls.
func (v *Verifier) Verify(
	candidate Candidate,
	observedOutput []byte,
	observedReceipt ReceiptFact,
	observedSources map[string]SourceMetaFact,
) Report {
	report := Report{Passed: true}

	digests := make(map[string]string, len(candidate.Sources))
	sourceParsed := make(map[string]*PCM16, len(candidate.Sources))
	for _, source := range candidate.Sources {
		sum := sha256.Sum256(source.WAV)
		digests[source.ID] = hex.EncodeToString(sum[:])
		parsed, err := ParseWAV(source.WAV)
		if err != nil {
			report.Passed = false
			report.Violations = append(report.Violations, Violation{
				Plane: PlanePipeline, Code: "unparseable_fixture",
				Detail: fmt.Sprintf("oracle cannot parse fixture source %s: %v", source.ID, err),
			})
			return report
		}
		sourceParsed[source.ID] = parsed
		v.checkSourceFacts(&report, source.ID, parsed, digests[source.ID], len(source.WAV), observedSources[source.ID])
	}

	prepared, totalFrames, err := prepare(candidate, digests)
	if err != nil {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlanePipeline, Code: "oracle_prepare_failed",
			Detail: err.Error(),
		})
		return report
	}

	channels := candidate.Timeline.OutputChannels
	mixed := mix(prepared, totalFrames, channels)
	normalization := normalizeAndQuantize(mixed, candidate.Timeline.TargetPeakDBFS)

	actual, err := ParseWAV(observedOutput)
	if err != nil {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlanePipeline, Code: "unparseable_output",
			Detail: "oracle cannot parse published output: " + err.Error(),
		})
		return report
	}
	report.OutputFrames = actual.FrameCount
	report.SampleRate = actual.SampleRate
	report.Channels = actual.Channels
	report.TotalSamples = len(actual.Quantized)
	report.AmbiguousSamples = normalization.ambiguousCount

	v.checkCoordinates(&report, candidate, prepared, totalFrames, observedReceipt)
	v.checkQuantizedFacts(&report, actual, normalization, observedOutput, observedReceipt)
	v.checkSignal(&report, actual, normalization)
	v.checkReceiptFacts(&report, candidate, prepared, normalization, observedReceipt, actual)

	if len(report.Violations) > 0 {
		report.Passed = false
	}
	return report
}

// checkSourceFacts independently verifies every registered source's stored
// digest, dimensions, frame count, and measurements against its raw bytes.
func (v *Verifier) checkSourceFacts(
	report *Report,
	sourceID string,
	parsed *PCM16,
	digest string,
	byteCount int,
	meta SourceMetaFact,
) {
	fail := func(code, detail string) {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneSource, Code: code, Span: -1, Detail: detail,
		})
	}
	if meta.ID != sourceID {
		fail("source_id_mismatch", fmt.Sprintf("source %s returned metadata id %q", sourceID, meta.ID))
	}
	if meta.Digest != digest {
		fail("source_digest_mismatch", fmt.Sprintf("source %s digest %s != sha256 of raw bytes %s", sourceID, meta.Digest, digest))
	}
	if meta.ByteCount != int64(byteCount) {
		fail("byte_count_mismatch", fmt.Sprintf("source %s byte count %d != %d", sourceID, meta.ByteCount, byteCount))
	}
	if meta.SampleRate != parsed.SampleRate || meta.Channels != parsed.Channels ||
		meta.BitsPerSample != 16 || meta.FrameCount != parsed.FrameCount {
		fail("source_dimensions_mismatch", fmt.Sprintf(
			"source %s reported rate=%d ch=%d bits=%d frames=%d, oracle rate=%d ch=%d frames=%d",
			sourceID, meta.SampleRate, meta.Channels, meta.BitsPerSample, meta.FrameCount,
			parsed.SampleRate, parsed.Channels, parsed.FrameCount))
	}
	// Match the production operation order exactly (multiply then divide) so
	// floating-point boundary cases are evaluated identically.
	expectedDuration := int64(math.Round(float64(parsed.FrameCount*1000) / float64(parsed.SampleRate)))
	if meta.DurationMS != expectedDuration {
		v.conventionViolation(report, Violation{
			Plane: PlaneSource, Code: "duration_mismatch", Span: -1,
			Expected: fmt.Sprintf("%d", expectedDuration), Actual: fmt.Sprintf("%d", meta.DurationMS),
			Detail: fmt.Sprintf("source %s duration %d != independent %d ms", sourceID, meta.DurationMS, expectedDuration),
		})
	}
	peak, rms := measureDB(parsed.Samples)
	if math.Abs(meta.PeakDBFS-peak) > v.DBTolerance {
		fail("peak_measurement_mismatch", fmt.Sprintf("source %s peak %.4f != oracle %.4f dBFS", sourceID, meta.PeakDBFS, peak))
	}
	if math.Abs(meta.RMSDBFS-rms) > v.DBTolerance {
		fail("rms_measurement_mismatch", fmt.Sprintf("source %s rms %.4f != oracle %.4f dBFS", sourceID, meta.RMSDBFS, rms))
	}
}

// checkCoordinates enforces exact integer geometry: output length, per-span
// output frames after rate conversion, and absolute start/end placement with
// crossfade overlap.
func (v *Verifier) checkCoordinates(
	report *Report,
	candidate Candidate,
	prepared []preparedSpan,
	totalFrames int64,
	receipt ReceiptFact,
) {
	if receipt.Output.FrameCount != totalFrames {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneCoordinates, Code: "output_frame_count",
			Expected: fmt.Sprintf("%d", totalFrames), Actual: fmt.Sprintf("%d", receipt.Output.FrameCount),
			Detail: "receipt output frame count disagrees with independent geometry",
		})
	}
	if len(receipt.Spans) != len(prepared) {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneCoordinates, Code: "span_count",
			Expected: fmt.Sprintf("%d", len(prepared)), Actual: fmt.Sprintf("%d", len(receipt.Spans)),
			Detail: "receipt span count disagrees with the submitted timeline",
		})
		return
	}
	for index, span := range prepared {
		observed := receipt.Spans[index]
		record := func(code, detail string) {
			report.Passed = false
			report.Violations = append(report.Violations, Violation{
				Plane: PlaneCoordinates, Code: code, Span: index, Detail: detail,
			})
		}
		if observed.SourceID != span.sourceID {
			record("span_source_id", fmt.Sprintf("span %d source %q != %q", index, observed.SourceID, span.sourceID))
		}
		if observed.SourceDigest != span.sourceDigest {
			record("span_source_digest", fmt.Sprintf("span %d digest %q != %q", index, observed.SourceDigest, span.sourceDigest))
		}
		if observed.SourceStartFrame != span.sourceStart || observed.SourceEndFrame != span.sourceEnd {
			record("span_source_interval", fmt.Sprintf(
				"span %d source interval [%d,%d] != submitted [%d,%d]",
				index, observed.SourceStartFrame, observed.SourceEndFrame, span.sourceStart, span.sourceEnd))
		}
		if observed.OutputFrames != span.outputFrames {
			record("span_output_frames", fmt.Sprintf(
				"span %d output frames %d != independently scaled %d", index, observed.OutputFrames, span.outputFrames))
		}
		if observed.OutputStartFrame != span.outputStart || observed.OutputEndFrame != span.outputEnd {
			record("span_placement", fmt.Sprintf(
				"span %d placement [%d,%d] != independent [%d,%d]",
				index, observed.OutputStartFrame, observed.OutputEndFrame, span.outputStart, span.outputEnd))
		}
		if observed.CrossfadeFrames != span.crossfadeFrames {
			record("span_crossfade", fmt.Sprintf(
				"span %d crossfade %d != %d", index, observed.CrossfadeFrames, span.crossfadeFrames))
		}
		if observed.ChannelAction != span.channelAction {
			record("span_channel_action", fmt.Sprintf(
				"span %d channel action %q != independent %q", index, observed.ChannelAction, span.channelAction))
		}
	}
}

// checkQuantizedFacts compares the actual int16 sample values, saturation
// behaviour, digest, and header facts. Boundary-straddling samples accept the
// immediately adjacent integer; every other sample must match exactly.
func (v *Verifier) checkQuantizedFacts(
	report *Report,
	actual *PCM16,
	normalization normalizationResult,
	outputBytes []byte,
	receipt ReceiptFact,
) {
	if len(actual.Quantized) != len(normalization.quantized) {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneQuantized, Code: "sample_count",
			Expected: fmt.Sprintf("%d", len(normalization.quantized)),
			Actual:   fmt.Sprintf("%d", len(actual.Quantized)),
			Detail:   "quantized sample count disagrees with independent render length",
		})
		return
	}
	ambiguousNeighbors := 0
	for index, expected := range normalization.quantized {
		got := actual.Quantized[index]
		if got == expected {
			continue
		}
		if normalization.ambiguous[index] && absInt16(got-expected) == 1 {
			ambiguousNeighbors++
			continue
		}
		frame := int64(index / actual.Channels)
		channel := index % actual.Channels
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneQuantized, Code: "quantized_sample",
			Frame: frame, Channel: channel,
			Expected: fmt.Sprintf("%d", expected), Actual: fmt.Sprintf("%d", got),
			Detail: "published int16 sample disagrees with independently quantized fact",
		})
		if len(report.Violations) > 24 {
			return
		}
	}
	// Saturation rails must agree exactly: clipping is a discrete fact.
	for index, q := range actual.Quantized {
		expected := normalization.quantized[index]
		actualRail := q == 32767 || q == -32768
		expectedRail := expected == 32767 || expected == -32768
		if actualRail != expectedRail && !(normalization.ambiguous[index] && absInt16(q-expected) == 1) {
			report.Passed = false
			report.Violations = append(report.Violations, Violation{
				Plane: PlaneQuantized, Code: "saturation",
				Frame: int64(index / actual.Channels), Channel: index % actual.Channels,
				Expected: fmt.Sprintf("%v", expectedRail), Actual: fmt.Sprintf("%v", actualRail),
				Detail: "saturation rail fact disagrees with independent expectation",
			})
		}
	}
	sum := sha256.Sum256(outputBytes)
	digest := hex.EncodeToString(sum[:])
	if receipt.Output.Digest != digest {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneQuantized, Code: "output_digest",
			Expected: digest, Actual: receipt.Output.Digest,
			Detail: "receipt digest is not the sha256 of the published WAV bytes",
		})
	}
	if actual.SampleRate != receipt.Output.SampleRate || actual.Channels != receipt.Output.Channels ||
		receipt.Output.BitsPerSample != 16 || actual.FrameCount != receipt.Output.FrameCount ||
		receipt.Output.ByteCount != int64(len(outputBytes)) {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneQuantized, Code: "wav_header_facts",
			Detail: fmt.Sprintf("output header facts rate=%d ch=%d bits=%d frames=%d bytes=%d disagree with receipt %+v",
				actual.SampleRate, actual.Channels, 16, actual.FrameCount, len(outputBytes), receipt.Output),
		})
	}
}

// checkSignal compares dequantized amplitudes against the independent float64
// mix. The tolerance is the documented quantization-plus-envelope bound.
func (v *Verifier) checkSignal(report *Report, actual *PCM16, normalization normalizationResult) {
	if len(actual.Samples) != len(normalization.normalized) {
		return // already reported on the quantized plane
	}
	// A high ambiguity rate would mean the envelope is hiding real divergence.
	ambiguityRate := float64(normalization.ambiguousCount) / float64(len(normalization.normalized))
	if ambiguityRate > 0.25 {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneSignal, Code: "ambiguity_budget_exceeded",
			Detail: fmt.Sprintf("%.1f%% of samples straddle quantization boundaries; envelope is not discriminating", ambiguityRate*100),
		})
	}
	for index, expected := range normalization.normalized {
		got := actual.Samples[index]
		if math.Abs(got-expected) <= v.SignalTolerance {
			continue
		}
		frame := int64(index / actual.Channels)
		channel := index % actual.Channels
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneSignal, Code: "amplitude",
			Frame: frame, Channel: channel,
			Expected: fmt.Sprintf("%.8f", expected), Actual: fmt.Sprintf("%.8f", got),
			Detail: fmt.Sprintf("dequantized amplitude differs by %.3e", math.Abs(got-expected)),
		})
		if len(report.Violations) > 48 {
			return
		}
	}
}

// checkReceiptFacts verifies normalization accounting, measurements, and
// metadata consistency on the receipt plane.
func (v *Verifier) checkReceiptFacts(
	report *Report,
	candidate Candidate,
	prepared []preparedSpan,
	normalization normalizationResult,
	receipt ReceiptFact,
	actual *PCM16,
) {
	fail := func(code, detail string) {
		report.Passed = false
		report.Violations = append(report.Violations, Violation{
			Plane: PlaneReceipt, Code: code, Detail: detail,
		})
	}
	if receipt.SchemaVersion != 1 {
		fail("schema_version", fmt.Sprintf("unexpected schema version %d", receipt.SchemaVersion))
	}
	if receipt.TimelineID != candidate.Timeline.ID {
		fail("timeline_id", fmt.Sprintf("receipt timeline %q != %q", receipt.TimelineID, candidate.Timeline.ID))
	}
	if math.Abs(receipt.Normalization.RequestedPeakDBFS-candidate.Timeline.TargetPeakDBFS) > 1e-9 {
		fail("requested_peak", "receipt does not echo the requested target peak")
	}
	beforeDB := toDB(normalization.beforePeak)
	if math.Abs(receipt.Normalization.BeforePeakDBFS-beforeDB) > v.DBTolerance {
		fail("before_peak", fmt.Sprintf("before-peak %.4f != oracle %.4f dBFS", receipt.Normalization.BeforePeakDBFS, beforeDB))
	}
	if math.Abs(receipt.Normalization.AppliedGainDB-normalization.gainDB) > v.DBTolerance {
		fail("applied_gain", fmt.Sprintf("applied gain %.4f != oracle %.4f dB", receipt.Normalization.AppliedGainDB, normalization.gainDB))
	}
	peak, rms := measureDB(actual.Samples)
	if math.Abs(receipt.Output.PeakDBFS-peak) > v.DBTolerance {
		fail("output_peak", fmt.Sprintf("receipt output peak %.4f != measured %.4f dBFS", receipt.Output.PeakDBFS, peak))
	}
	if math.Abs(receipt.Output.RMSDBFS-rms) > v.DBTolerance {
		fail("output_rms", fmt.Sprintf("receipt output rms %.4f != measured %.4f dBFS", receipt.Output.RMSDBFS, rms))
	}
	if math.Abs(receipt.Normalization.OutputPeakDBFS-peak) > v.DBTolerance {
		fail("normalization_output_peak", fmt.Sprintf("normalization output peak %.4f != measured %.4f dBFS", receipt.Normalization.OutputPeakDBFS, peak))
	}
	duration := int64(math.Round(float64(actual.FrameCount) * 1000 / float64(actual.SampleRate)))
	if receipt.Output.DurationMS != duration {
		// Known production convention: render receipts truncate the frame
		// duration while source metadata rounds it. Recorded independently;
		// only strict convention mode treats it as a hard failure.
		v.conventionViolation(report, Violation{
			Plane: PlaneReceipt, Code: "output_duration",
			Expected: fmt.Sprintf("%d", duration), Actual: fmt.Sprintf("%d", receipt.Output.DurationMS),
			Detail: fmt.Sprintf("receipt duration %d truncates where independent rounding gives %d ms", receipt.Output.DurationMS, duration),
		})
	}
	// The target peak must actually be honored unless the signal is silent.
	if normalization.beforePeak > 0 && math.Abs(peak-candidate.Timeline.TargetPeakDBFS) > 0.25 {
		fail("target_peak_not_honored", fmt.Sprintf("measured peak %.4f dBFS misses target %.4f", peak, candidate.Timeline.TargetPeakDBFS))
	}
	expectedWarnings := 0
	for _, span := range prepared {
		if span.channelAction != "unchanged" {
			expectedWarnings++
		}
	}
	if len(receipt.Warnings) != expectedWarnings {
		fail("warnings", fmt.Sprintf("receipt carries %d warnings, oracle expects %d channel-mapping warnings", len(receipt.Warnings), expectedWarnings))
	}
}

func measureDB(samples []float64) (peakDB, rmsDB float64) {
	peak := 0.0
	sumSquares := 0.0
	for _, sample := range samples {
		absolute := math.Abs(sample)
		if absolute > peak {
			peak = absolute
		}
		sumSquares += sample * sample
	}
	rms := math.Sqrt(sumSquares / float64(len(samples)))
	return toDB(peak), toDB(rms)
}

func absInt16(value int16) int {
	if value < 0 {
		return int(-value)
	}
	return int(value)
}
