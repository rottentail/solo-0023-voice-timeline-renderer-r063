package oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"
)

// makeWAV builds a PCM16 WAV entirely through the oracle's own encoder so the
// test never depends on production codec code.
func makeWAV(t *testing.T, rate, channels int, samples []float64) []byte {
	t.Helper()
	data, err := EncodeWAV(rate, channels, samples)
	if err != nil {
		t.Fatalf("EncodeWAV: %v", err)
	}
	return data
}

func TestParseWAVRoundTrip(t *testing.T) {
	original := []float64{0.5, -0.5, 1, -1, 0.123456, -0.123456}
	data := makeWAV(t, 44100, 2, original)
	parsed, err := ParseWAV(data)
	if err != nil {
		t.Fatalf("ParseWAV: %v", err)
	}
	if parsed.SampleRate != 44100 || parsed.Channels != 2 || parsed.FrameCount != 3 {
		t.Fatalf("unexpected header %+v", parsed)
	}
	// Rails and asymmetric dequantization must be exact.
	if parsed.Quantized[2] != 32767 || parsed.Quantized[3] != -32768 {
		t.Fatalf("rail quantization wrong: %d %d", parsed.Quantized[2], parsed.Quantized[3])
	}
	if math.Abs(parsed.Samples[0]-0.5) > 1.0/pcm16MaxPositive {
		t.Fatalf("dequantize mismatch: %.8f", parsed.Samples[0])
	}
}

func TestQuantizeOneRoundingAndClamp(t *testing.T) {
	cases := []struct {
		in  float64
		out int16
	}{
		{1.0, 32767},
		{1.5, 32767},
		{-1.0, -32768},
		{-1.5, -32768},
		{0.0, 0},
		{0.5 / 32767.0, 1},
		{-0.5 / 32768.0, -1},
	}
	for _, tc := range cases {
		if got := QuantizeOne(tc.in); got != tc.out {
			t.Errorf("QuantizeOne(%v) = %d, want %d", tc.in, got, tc.out)
		}
	}
}

func TestResampleFrameCountAndIdentity(t *testing.T) {
	// 48000 -> 24000 halves the frame count.
	samples := make([]float64, 480) // 480 mono frames at 48k = 10 ms
	for i := range samples {
		samples[i] = float64(i) / 480
	}
	source := &clip{rate: 48000, channels: 1, samples: samples}
	down := resample(source, 24000)
	if down.frames() != 240 {
		t.Fatalf("downsample frames = %d, want 240", down.frames())
	}
	// Same rate returns an equal-length copy, never the same backing array.
	same := resample(source, 48000)
	if same.frames() != source.frames() || &same.samples[0] == &source.samples[0] {
		t.Fatal("same-rate resample must return an independent copy")
	}
}

func TestResampleInterpolatesLinearly(t *testing.T) {
	// Two frames [0, 1] at rate 100 resampled to 200: positions 0, 0.5 map to
	// values 0 and 0.5 via linear interpolation.
	source := &clip{rate: 100, channels: 1, samples: []float64{0, 1}}
	up := resample(source, 200)
	if len(up.samples) != 4 {
		t.Fatalf("frame count = %d, want 4", len(up.samples))
	}
	if math.Abs(up.samples[1]-0.5) > 1e-9 {
		t.Fatalf("interpolated value = %.6f, want 0.5", up.samples[1])
	}
}

func TestMapChannels(t *testing.T) {
	mono := &clip{rate: 48000, channels: 1, samples: []float64{0.4}}
	stereo, action := mapChannels(mono, 2)
	if action != "mono_to_stereo" || stereo.samples[0] != 0.4 || stereo.samples[1] != 0.4 {
		t.Fatalf("mono->stereo wrong: %+v %s", stereo.samples, action)
	}
	back, action := mapChannels(stereo, 1)
	if action != "stereo_to_mono" || math.Abs(back.samples[0]-0.4) > 1e-9 {
		t.Fatalf("stereo->mono wrong: %+v %s", back.samples, action)
	}
}

func TestMixEqualPowerCrossfade(t *testing.T) {
	// Two constant 0.5 spans overlapping by one frame. At frame 0 of the second
	// span, equal-power weights combine both contributions.
	spanA := preparedSpan{
		index: 0, outputStart: 0, outputEnd: 2, outputFrames: 2,
		gainDB: 0, clip: &clip{rate: 100, channels: 1, samples: []float64{0.5, 0.5}},
	}
	spanB := preparedSpan{
		index: 1, outputStart: 1, outputEnd: 3, outputFrames: 2,
		crossfadeFrames: 1, gainDB: 0,
		clip: &clip{rate: 100, channels: 1, samples: []float64{0.5, 0.5}},
	}
	mixed := mix([]preparedSpan{spanA, spanB}, 3, 1)
	// At the overlap frame: 0.5*cos(pi/4) + 0.5*sin(pi/4).
	want := 0.5*math.Cos(math.Pi/4) + 0.5*math.Sin(math.Pi/4)
	if math.Abs(mixed[1]-want) > 1e-9 {
		t.Fatalf("crossfade mix = %.8f, want %.8f", mixed[1], want)
	}
	if math.Abs(mixed[0]-0.5) > 1e-9 || math.Abs(mixed[2]-0.5) > 1e-9 {
		t.Fatalf("non-overlap frames altered: %+v", mixed)
	}
}

// buildSingleSpanCandidate renders one mono source through one span so the
// expected output is the source itself scaled only by normalization.
func buildSingleSpanCandidate(t *testing.T) (Candidate, map[string]SourceMetaFact) {
	t.Helper()
	samples := make([]float64, 100)
	for i := range samples {
		samples[i] = 0.6
	}
	wav := makeWAV(t, 48000, 1, samples)
	candidate := Candidate{
		Sources: []Source{{ID: "s1", WAV: wav}},
		Timeline: Timeline{
			ID: "t1", SampleRate: 48000, OutputChannels: 1, TargetPeakDBFS: -6,
			Spans: []Span{{SourceID: "s1", SourceStartFrame: 0, SourceEndFrame: 100, GainDB: 0}},
		},
	}
	sum := sha256.Sum256(wav)
	meta := map[string]SourceMetaFact{
		"s1": {
			ID: "s1", Digest: hex.EncodeToString(sum[:]), ByteCount: int64(len(wav)),
			SampleRate: 48000, Channels: 1, BitsPerSample: 16, FrameCount: 100,
			DurationMS: 2, PeakDBFS: toDB(0.6), RMSDBFS: toDB(0.6),
		},
	}
	return candidate, meta
}

// buildExpectedOutput renders the candidate through the oracle pipeline and
// returns the encoded WAV plus a receipt measured from the actual output, so
// the test asserts the verifier's mechanism rather than hardcoded constants.
func buildExpectedOutput(t *testing.T, candidate Candidate, renderID string) ([]byte, ReceiptFact) {
	t.Helper()
	digests := make(map[string]string, len(candidate.Sources))
	for _, source := range candidate.Sources {
		sum := sha256.Sum256(source.WAV)
		digests[source.ID] = hex.EncodeToString(sum[:])
	}
	prepared, total, err := prepare(candidate, digests)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	mixed := mix(prepared, total, candidate.Timeline.OutputChannels)
	norm := normalizeAndQuantize(mixed, candidate.Timeline.TargetPeakDBFS)
	output, err := EncodeWAV(candidate.Timeline.SampleRate, candidate.Timeline.OutputChannels, norm.normalized)
	if err != nil {
		t.Fatalf("EncodeWAV: %v", err)
	}
	parsed, err := ParseWAV(output)
	if err != nil {
		t.Fatalf("ParseWAV: %v", err)
	}
	peak, rms := measureDB(parsed.Samples)
	sum := sha256.Sum256(output)
	spans := make([]ReceiptSpanFact, 0, len(prepared))
	for _, span := range prepared {
		spans = append(spans, ReceiptSpanFact{
			Index: span.index, SourceID: span.sourceID, SourceDigest: span.sourceDigest,
			SourceStartFrame: span.sourceStart, SourceEndFrame: span.sourceEnd,
			OutputStartFrame: span.outputStart, OutputEndFrame: span.outputEnd,
			OutputFrames: span.outputFrames, CrossfadeFrames: span.crossfadeFrames,
			GainDB: span.gainDB, ChannelAction: span.channelAction,
		})
	}
	receipt := ReceiptFact{
		SchemaVersion: 1, RenderID: renderID, TimelineID: candidate.Timeline.ID,
		Output: OutputFact{
			Digest: hex.EncodeToString(sum[:]), ByteCount: int64(len(output)),
			SampleRate: parsed.SampleRate, Channels: parsed.Channels, BitsPerSample: 16,
			FrameCount: parsed.FrameCount, DurationMS: int64(math.Round(float64(parsed.FrameCount*1000) / float64(parsed.SampleRate))),
			PeakDBFS: peak, RMSDBFS: rms,
		},
		Normalization: NormalizationFact{
			RequestedPeakDBFS: candidate.Timeline.TargetPeakDBFS,
			BeforePeakDBFS:    toDB(norm.beforePeak),
			AppliedGainDB:     norm.gainDB,
			OutputPeakDBFS:    peak,
		},
		Spans: spans,
	}
	return output, receipt
}

func TestVerifyAcceptsConsistentRender(t *testing.T) {
	candidate, meta := buildSingleSpanCandidate(t)
	output, receipt := buildExpectedOutput(t, candidate, "r1")
	report := NewVerifier().Verify(candidate, output, receipt, meta)
	if !report.Passed {
		t.Fatalf("expected clean report, got violations: %+v", report.Violations)
	}
}

func TestVerifyDetectsTamperedQuantizedSample(t *testing.T) {
	candidate, meta := buildSingleSpanCandidate(t)
	prepared, total, _ := prepare(candidate, map[string]string{"s1": meta["s1"].Digest})
	mixed := mix(prepared, total, 1)
	norm := normalizeAndQuantize(mixed, -6)
	// Corrupt one sample well outside the ambiguity envelope.
	norm.normalized[10] += 0.05
	output, _ := EncodeWAV(48000, 1, norm.normalized)
	sum := sha256.Sum256(output)
	parsed, _ := ParseWAV(output)
	peak, rms := measureDB(parsed.Samples)
	receipt := ReceiptFact{
		SchemaVersion: 1, RenderID: "r1", TimelineID: "t1",
		Output: OutputFact{
			Digest: hex.EncodeToString(sum[:]), ByteCount: int64(len(output)),
			SampleRate: 48000, Channels: 1, BitsPerSample: 16, FrameCount: total, DurationMS: 2,
			PeakDBFS: peak, RMSDBFS: rms,
		},
		Normalization: NormalizationFact{RequestedPeakDBFS: -6},
		Spans: []ReceiptSpanFact{{
			Index: 0, SourceID: "s1", SourceDigest: meta["s1"].Digest,
			SourceEndFrame: 100, OutputEndFrame: 100, OutputFrames: 100, ChannelAction: "unchanged",
		}},
	}
	report := NewVerifier().Verify(candidate, output, receipt, meta)
	if report.Passed {
		t.Fatal("expected violations for tampered sample, got clean report")
	}
	planes := map[Plane]bool{}
	for _, violation := range report.Violations {
		planes[violation.Plane] = true
	}
	if !planes[PlaneSignal] || !planes[PlaneQuantized] {
		t.Fatalf("expected signal and quantized violations, got %+v", report.Violations)
	}
}

func TestVerifyDetectsCoordinateDrift(t *testing.T) {
	candidate, meta := buildSingleSpanCandidate(t)
	prepared, total, _ := prepare(candidate, map[string]string{"s1": meta["s1"].Digest})
	mixed := mix(prepared, total, 1)
	norm := normalizeAndQuantize(mixed, -6)
	output, _ := EncodeWAV(48000, 1, norm.normalized)
	sum := sha256.Sum256(output)
	parsed, _ := ParseWAV(output)
	peak, rms := measureDB(parsed.Samples)
	// Lie about frame geometry on the receipt.
	receipt := ReceiptFact{
		SchemaVersion: 1, RenderID: "r1", TimelineID: "t1",
		Output: OutputFact{
			Digest: hex.EncodeToString(sum[:]), ByteCount: int64(len(output)),
			SampleRate: 48000, Channels: 1, BitsPerSample: 16, FrameCount: total + 7,
			DurationMS: 2, PeakDBFS: peak, RMSDBFS: rms,
		},
		Spans: []ReceiptSpanFact{{
			Index: 0, SourceID: "s1", SourceDigest: meta["s1"].Digest,
			SourceEndFrame: 100, OutputEndFrame: 107, OutputFrames: 107, ChannelAction: "unchanged",
		}},
	}
	report := NewVerifier().Verify(candidate, output, receipt, meta)
	var hasCoordinate bool
	for _, violation := range report.Violations {
		if violation.Plane == PlaneCoordinates {
			hasCoordinate = true
		}
	}
	if !hasCoordinate {
		t.Fatalf("expected coordinate violation, got %+v", report.Violations)
	}
}

func TestStrictConventionModeFailsOnDurationConvention(t *testing.T) {
	candidate, meta := buildSingleSpanCandidate(t)
	// Build a deliberately short source whose rounded duration is 1 ms but the
	// truncation convention reports 0 ms.
	shortSamples := make([]float64, 48) // 1 ms at 48k
	for i := range shortSamples {
		shortSamples[i] = 0.6
	}
	shortWAV := makeWAV(t, 48000, 1, shortSamples)
	candidate.Sources[0].WAV = shortWAV
	candidate.Timeline.Spans[0].SourceEndFrame = 48
	sourceMeta := meta["s1"]
	sourceMeta.FrameCount = 48
	sourceMeta.DurationMS = 1
	sum := sha256.Sum256(shortWAV)
	sourceMeta.Digest = hex.EncodeToString(sum[:])
	sourceMeta.ByteCount = int64(len(shortWAV))
	meta["s1"] = sourceMeta
	output, receipt := buildExpectedOutput(t, candidate, "r1")
	// Apply the render-receipt truncation convention.
	receipt.Output.DurationMS = 0

	lenient := NewVerifier()
	lenientReport := lenient.Verify(candidate, output, receipt, meta)
	if !lenientReport.Passed {
		t.Fatalf("lenient mode should pass, got %+v", lenientReport.Violations)
	}
	if len(lenientReport.Observations) == 0 {
		t.Fatal("lenient mode must still record the convention observation")
	}

	strict := NewVerifier()
	strict.StrictConventions = true
	strictReport := strict.Verify(candidate, output, receipt, meta)
	if strictReport.Passed {
		t.Fatal("strict convention mode must fail on the duration convention")
	}
	found := false
	for _, violation := range strictReport.Violations {
		if violation.Plane == PlaneReceipt && violation.Code == "output_duration" {
			found = true
		}
	}
	if !found {
		t.Fatalf("strict report missing output_duration violation: %+v", strictReport.Violations)
	}
}

func TestVerifyDetectsDigestMismatch(t *testing.T) {
	candidate, meta := buildSingleSpanCandidate(t)
	prepared, total, _ := prepare(candidate, map[string]string{"s1": meta["s1"].Digest})
	mixed := mix(prepared, total, 1)
	norm := normalizeAndQuantize(mixed, -6)
	output, _ := EncodeWAV(48000, 1, norm.normalized)
	parsed, _ := ParseWAV(output)
	peak, rms := measureDB(parsed.Samples)
	receipt := ReceiptFact{
		SchemaVersion: 1, RenderID: "r1", TimelineID: "t1",
		Output: OutputFact{
			Digest: "deadbeef", ByteCount: int64(len(output)),
			SampleRate: 48000, Channels: 1, BitsPerSample: 16, FrameCount: total, DurationMS: 2,
			PeakDBFS: peak, RMSDBFS: rms,
		},
		Spans: []ReceiptSpanFact{{
			Index: 0, SourceID: "s1", SourceDigest: meta["s1"].Digest,
			SourceEndFrame: 100, OutputEndFrame: 100, OutputFrames: 100, ChannelAction: "unchanged",
		}},
	}
	report := NewVerifier().Verify(candidate, output, receipt, meta)
	var hasDigest bool
	for _, violation := range report.Violations {
		if violation.Plane == PlaneQuantized && violation.Code == "output_digest" {
			hasDigest = true
		}
	}
	if !hasDigest {
		t.Fatalf("expected digest violation, got %+v", report.Violations)
	}
}
