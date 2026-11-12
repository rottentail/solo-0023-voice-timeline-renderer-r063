package oracle

import "math"

// clip is the oracle's working audio buffer. It is deliberately distinct from
// the production Clip type.
type clip struct {
	rate     int
	channels int
	samples  []float64
}

func (c *clip) frames() int64 {
	if c == nil || c.channels == 0 {
		return 0
	}
	return int64(len(c.samples) / c.channels)
}

func slice(c *clip, startFrame, endFrame int64) *clip {
	start := int(startFrame) * c.channels
	end := int(endFrame) * c.channels
	out := make([]float64, end-start)
	copy(out, c.samples[start:end])
	return &clip{rate: c.rate, channels: c.channels, samples: out}
}

// resample re-derives linear sample-rate conversion: the output frame count is
// the rounded length ratio and each output position maps to a fractional input
// position interpolated between its two neighbours with edge clamping.
func resample(c *clip, targetRate int) *clip {
	sourceFrames := c.frames()
	targetFrames := int64(math.Round(float64(sourceFrames) * float64(targetRate) / float64(c.rate)))
	if targetFrames < 1 {
		targetFrames = 1
	}
	if sourceFrames == targetFrames && c.rate == targetRate {
		out := make([]float64, len(c.samples))
		copy(out, c.samples)
		return &clip{rate: targetRate, channels: c.channels, samples: out}
	}
	out := make([]float64, int(targetFrames)*c.channels)
	step := float64(c.rate) / float64(targetRate)
	for frame := int64(0); frame < targetFrames; frame++ {
		position := float64(frame) * step
		left := int64(position)
		if left >= sourceFrames-1 {
			left = sourceFrames - 1
		}
		right := left + 1
		if right >= sourceFrames {
			right = sourceFrames - 1
		}
		weight := position - float64(left)
		for ch := 0; ch < c.channels; ch++ {
			a := c.samples[int(left)*c.channels+ch]
			b := c.samples[int(right)*c.channels+ch]
			out[int(frame)*c.channels+ch] = a + (b-a)*weight
		}
	}
	return &clip{rate: targetRate, channels: c.channels, samples: out}
}

// mapChannels re-derives layout conversion: mono duplicates into both output
// channels, stereo downmixes by averaging.
func mapChannels(c *clip, outputChannels int) (*clip, string) {
	if c.channels == outputChannels {
		out := make([]float64, len(c.samples))
		copy(out, c.samples)
		return &clip{rate: c.rate, channels: outputChannels, samples: out}, "unchanged"
	}
	frames := c.frames()
	out := make([]float64, int(frames)*outputChannels)
	action := "mono_to_stereo"
	if outputChannels == 1 {
		action = "stereo_to_mono"
	}
	for frame := int64(0); frame < frames; frame++ {
		if c.channels == 1 && outputChannels == 2 {
			value := c.samples[int(frame)]
			out[int(frame)*2] = value
			out[int(frame)*2+1] = value
			continue
		}
		left := c.samples[int(frame)*2]
		right := c.samples[int(frame)*2+1]
		out[int(frame)] = (left + right) * 0.5
	}
	return &clip{rate: c.rate, channels: outputChannels, samples: out}, action
}

type preparedSpan struct {
	index           int
	sourceID        string
	sourceDigest    string
	sourceStart     int64
	sourceEnd       int64
	outputStart     int64
	outputEnd       int64
	outputFrames    int64
	crossfadeFrames int64
	gainDB          float64
	channelAction   string
	clip            *clip
}

// prepare resolves every span independently from the raw source WAV bytes.
func prepare(candidate Candidate, digests map[string]string) ([]preparedSpan, int64, error) {
	sourceClips := make(map[string]*clip, len(candidate.Sources))
	for _, source := range candidate.Sources {
		parsed, err := ParseWAV(source.WAV)
		if err != nil {
			return nil, 0, err
		}
		sourceClips[source.ID] = &clip{
			rate:     parsed.SampleRate,
			channels: parsed.Channels,
			samples:  parsed.Samples,
		}
	}
	prepared := make([]preparedSpan, 0, len(candidate.Timeline.Spans))
	cursor := int64(0)
	for index, span := range candidate.Timeline.Spans {
		sourceClip, ok := sourceClips[span.SourceID]
		if !ok {
			return nil, 0, errUnknownSource(span.SourceID)
		}
		selected := slice(sourceClip, span.SourceStartFrame, span.SourceEndFrame)
		rateAdjusted := resample(selected, candidate.Timeline.SampleRate)
		mapped, action := mapChannels(rateAdjusted, candidate.Timeline.OutputChannels)
		frames := mapped.frames()
		start := cursor
		if index > 0 {
			start -= span.CrossfadeFrames
		}
		end := start + frames
		cursor = end
		prepared = append(prepared, preparedSpan{
			index:           index,
			sourceID:        span.SourceID,
			sourceDigest:    digests[span.SourceID],
			sourceStart:     span.SourceStartFrame,
			sourceEnd:       span.SourceEndFrame,
			outputStart:     start,
			outputEnd:       end,
			outputFrames:    frames,
			crossfadeFrames: span.CrossfadeFrames,
			gainDB:          span.GainDB,
			channelAction:   action,
			clip:            mapped,
		})
	}
	return prepared, cursor, nil
}

// mix re-derives the equal-power overlap accumulation. The incoming span is
// weighted by sin and the previously accumulated tail by cos over the
// crossfade region; gains are applied in linear amplitude.
func mix(spans []preparedSpan, totalFrames int64, channels int) []float64 {
	mixed := make([]float64, int(totalFrames)*channels)
	for _, span := range spans {
		gain := math.Pow(10, span.gainDB/20)
		frames := span.clip.frames()
		for frame := int64(0); frame < frames; frame++ {
			incoming := 1.0
			outgoing := 1.0
			if span.crossfadeFrames > 0 && frame < span.crossfadeFrames {
				progress := (float64(frame) + 0.5) / float64(span.crossfadeFrames)
				angle := progress * math.Pi / 2
				incoming = math.Sin(angle)
				outgoing = math.Cos(angle)
			}
			inStart := int(frame) * channels
			outStart := int(span.outputStart+frame) * channels
			for ch := 0; ch < channels; ch++ {
				value := span.clip.samples[inStart+ch] * gain * incoming
				if span.crossfadeFrames > 0 && frame < span.crossfadeFrames {
					mixed[outStart+ch] *= outgoing
				}
				mixed[outStart+ch] += value
			}
		}
	}
	return mixed
}

type normalizationResult struct {
	gain           float64
	gainDB         float64
	beforePeak     float64
	normalized     []float64
	quantized      []int16
	ambiguous      []bool
	ambiguousCount int
}

// normalizeAndQuantize re-derives peak normalization and PCM16 quantization.
// For every output sample it also records whether the pre-quantization value
// sits within the floating-point envelope of a bin boundary, in which case an
// adjacent integer is an acceptable quantization fact.
func normalizeAndQuantize(mixed []float64, targetPeakDBFS float64) normalizationResult {
	peak := 0.0
	for _, value := range mixed {
		if abs := math.Abs(value); abs > peak {
			peak = abs
		}
	}
	target := math.Pow(10, targetPeakDBFS/20)
	gain := 1.0
	if peak > 0 {
		gain = target / peak
	}
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain <= 0 {
		gain = 1
	}
	normalized := make([]float64, len(mixed))
	quantized := make([]int16, len(mixed))
	ambiguous := make([]bool, len(mixed))
	ambiguousCount := 0
	// The production pipeline accumulates every stage in float32 (resampling,
	// channel mapping, crossfade weights, gain, normalization) before rounding
	// to int16. A value is quantization-ambiguous only when the float64
	// expectation sits within the accumulated float32 envelope of a bin
	// boundary, where the adjacent integer is an equally valid fact. The
	// envelope stays well under one quantization bin (~3.05e-5) so genuine
	// multi-LSB divergence is never excused.
	const floatEnvelope = 2.0e-6
	for index, sample := range mixed {
		value := sample * gain
		if value > 1 {
			value = 1
		}
		if value < -1 {
			value = -1
		}
		normalized[index] = value
		quantized[index] = QuantizeOne(value)
		scale := binScale(value)
		binWidth := 1.0 / scale
		scaled := math.Abs(value) * scale
		// Distance to the nearest half-integer boundary, expressed in
		// normalized amplitude units so it is comparable to the envelope.
		boundaryDistance := math.Abs(scaled-(math.Floor(scaled)+0.5)) * binWidth
		if boundaryDistance <= floatEnvelope {
			ambiguous[index] = true
			ambiguousCount++
		}
	}
	return normalizationResult{
		gain:           gain,
		gainDB:         toDB(gain),
		beforePeak:     peak,
		normalized:     normalized,
		quantized:      quantized,
		ambiguous:      ambiguous,
		ambiguousCount: ambiguousCount,
	}
}

func binScale(value float64) float64 {
	if value < 0 {
		return pcm16MinNegative
	}
	return pcm16MaxPositive
}

func toDB(linear float64) float64 {
	if linear <= 0 {
		return -120
	}
	db := 20 * math.Log10(linear)
	if db < -120 {
		return -120
	}
	return db
}

type namedError struct{ message string }

func (e namedError) Error() string { return e.message }

func errUnknownSource(id string) error { return namedError{"unknown source: " + id} }
