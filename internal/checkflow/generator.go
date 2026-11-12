package checkflow

import (
	"math"
	"math/rand/v2"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/oracle"
)

// GeneratorConfig bounds every produced candidate so a single experiment can
// never exceed the process resource budget on its own. These are structural
// generation caps, not tolerance relaxations: every candidate still receives
// full three-plane verification.
type GeneratorConfig struct {
	MaxSources       int
	MaxSpans         int
	MaxSourceFrames  int64
	MinSourceFrames  int64
	MaxOutputFrames  int64
	MaxSourceWAVSize int
}

// DefaultGeneratorConfig is calibrated for the 256 MiB RSS budget with two
// parallel workers. Each source stays below ~6 MiB of PCM and every render
// stays below ~9.6 MiB of float32 mix plus encoding copies.
func DefaultGeneratorConfig() GeneratorConfig {
	return GeneratorConfig{
		MaxSources:       8,
		MaxSpans:         24,
		MinSourceFrames:  96,
		MaxSourceFrames:  160000,
		MaxOutputFrames:  240000,
		MaxSourceWAVSize: 6 << 20,
	}
}

// sampleRates deliberately mixes rates whose ratios produce fractional frame
// landing points, which is where resampling and crossfade placement errors
// show up.
var sampleRates = []int{8000, 11025, 16000, 22050, 32000, 44100, 48000, 88200, 96000}

// Generator synthesizes bounded, deterministic render candidates from a
// seeded PRNG. Generated signals include impulses and phase-inverted pairs so
// fractional landing points and destructive crossfade cancellation have
// observable markers.
type Generator struct {
	config GeneratorConfig
	rng    *rand.Rand
}

func NewGenerator(config GeneratorConfig, seed uint64) *Generator {
	return &Generator{config: config, rng: rand.New(rand.NewChaCha8([32]byte{byte(seed), byte(seed >> 8), byte(seed >> 16), byte(seed >> 24)}))}
}

// GeneratedCandidate bundles the oracle contract with the HTTP request shapes
// derived from it, so nothing about a candidate is reconstructed implicitly.
type GeneratedCandidate struct {
	Oracle       oracle.Candidate
	SourceWAVs   map[string][]byte
	SourceMeta   map[string]generatedSource
	Spans        []domain.TimelineSpan
	SampleRate   int
	Channels     int
	TargetPeak   float64
	Large        bool
	MarkerFrames []int64
	OutputFrames int64
}

// Next produces one bounded candidate, including a deliberately large legal
// project whenever large is true. Large projects are never skipped: they go
// through the same complete verification path.
func (g *Generator) Next(sequence int64, large bool) GeneratedCandidate {
	rng := g.rng
	cfg := g.config
	sourceCount := 1 + rng.IntN(cfg.MaxSources)
	if large {
		sourceCount = cfg.MaxSources
	}

	oracleSources := make([]oracle.Source, 0, sourceCount)
	sourceWAVs := make(map[string][]byte, sourceCount)
	sourceMeta := make(map[string]generatedSource, sourceCount)
	markerFrames := make([]int64, 0)

	for index := 0; index < sourceCount; index++ {
		id := candidateSourceID(sequence, index)
		rate := sampleRates[rng.IntN(len(sampleRates))]
		channels := 1 + rng.IntN(2)
		frames := g.sourceFrames(large)
		waveform := rng.IntN(6)
		samples, markers := g.synthesize(frames, channels, waveform, sequence, int64(index))
		wav, err := oracle.EncodeWAV(rate, channels, samples)
		if err != nil {
			// EncodeWAV only fails on out-of-range structural inputs that the
			// generator never emits; treat it as a deterministic invariant.
			panic(err)
		}
		if len(wav) > cfg.MaxSourceWAVSize {
			frames = (cfg.MaxSourceWAVSize - 44) / (2 * channels)
			samples, markers = g.synthesize(frames, channels, waveform, sequence, int64(index))
			wav, err = oracle.EncodeWAV(rate, channels, samples)
			if err != nil {
				panic(err)
			}
		}
		sourceWAVs[id] = wav
		oracleSources = append(oracleSources, oracle.Source{ID: id, WAV: wav})
		sourceMeta[id] = generatedSource{rate: rate, channels: channels, frames: int64(frames)}
		for _, marker := range markers {
			markerFrames = append(markerFrames, marker)
		}
	}

	timelineRate := sampleRates[rng.IntN(len(sampleRates))]
	outputChannels := 1 + rng.IntN(2)
	targetPeak := -24 + rng.Float64()*23.9 // within [-24, -0.1)
	spanCount := 1 + rng.IntN(cfg.MaxSpans)
	if large {
		spanCount = cfg.MaxSpans
	}
	ids := make([]string, 0, sourceCount)
	for id := range sourceMeta {
		ids = append(ids, id)
	}

	spans := make([]domain.TimelineSpan, 0, spanCount)
	oracleSpans := make([]oracle.Span, 0, spanCount)
	cursorOutput := int64(0)
	committedCursor := int64(0)
	var previousOutputFrames int64
	for index := 0; index < spanCount; index++ {
		sourceID := ids[rng.IntN(len(ids))]
		meta := sourceMeta[sourceID]
		interval := g.sourceInterval(meta.frames)
		outputFrames := scaledFrames(interval, meta.rate, timelineRate)
		crossfade := int64(0)
		if index > 0 {
			upper := previousOutputFrames
			if outputFrames < upper {
				upper = outputFrames
			}
			if upper > 1 {
				// bias toward non-trivial overlaps so cancellation relations
				// are exercised frequently.
				crossfade = rng.Int64N(upper)
			}
		}
		gain := -36 + rng.Float64()*48 // within [-36, 12)
		nextCursor := cursorOutput + outputFrames
		if index > 0 {
			nextCursor -= crossfade
		}
		if nextCursor > cfg.MaxOutputFrames {
			// The output cap is a hard structural bound: stop extending rather
			// than appending an over-limit span.
			break
		}
		spans = append(spans, domain.TimelineSpan{
			SourceID:         sourceID,
			SourceStartFrame: interval.start,
			SourceEndFrame:   interval.end,
			GainDB:           gain,
			CrossfadeFrames:  crossfade,
		})
		oracleSpans = append(oracleSpans, oracle.Span{
			SourceID:         sourceID,
			SourceStartFrame: interval.start,
			SourceEndFrame:   interval.end,
			GainDB:           gain,
			CrossfadeFrames:  crossfade,
		})
		cursorOutput = nextCursor
		committedCursor = nextCursor
		previousOutputFrames = outputFrames
	}
	if len(spans) == 0 {
		// Guarantee at least one legal span even under tight output caps. The
		// interval is explicitly bounded so its rate-scaled output length
		// cannot exceed MaxOutputFrames regardless of the rate ratio.
		sourceID := ids[0]
		meta := sourceMeta[sourceID]
		interval := g.boundedInterval(meta, timelineRate)
		spans = append(spans, domain.TimelineSpan{
			SourceID: sourceID, SourceStartFrame: interval.start, SourceEndFrame: interval.end,
			GainDB: -6, CrossfadeFrames: 0,
		})
		oracleSpans = append(oracleSpans, oracle.Span{
			SourceID: sourceID, SourceStartFrame: interval.start, SourceEndFrame: interval.end,
			GainDB: -6, CrossfadeFrames: 0,
		})
		committedCursor = scaledFrames(interval, meta.rate, timelineRate)
	}

	timelineID := candidateTimelineID(sequence)
	return GeneratedCandidate{
		Oracle: oracle.Candidate{
			Sources: oracleSources,
			Timeline: oracle.Timeline{
				ID:             timelineID,
				SampleRate:     timelineRate,
				OutputChannels: outputChannels,
				TargetPeakDBFS: targetPeak,
				Spans:          oracleSpans,
			},
		},
		SourceWAVs:   sourceWAVs,
		SourceMeta:   sourceMeta,
		Spans:        spans,
		SampleRate:   timelineRate,
		Channels:     outputChannels,
		TargetPeak:   targetPeak,
		Large:        large,
		MarkerFrames: markerFrames,
		OutputFrames: committedCursor,
	}
}

type frameInterval struct{ start, end int64 }

func (g *Generator) sourceInterval(sourceFrames int64) frameInterval {
	maxLen := sourceFrames
	if maxLen > g.config.MaxSourceFrames {
		maxLen = g.config.MaxSourceFrames
	}
	length := g.config.MinSourceFrames + g.rng.Int64N(maxLen-g.config.MinSourceFrames+1)
	if length > sourceFrames {
		length = sourceFrames
	}
	start := int64(0)
	if sourceFrames > length {
		start = g.rng.Int64N(sourceFrames - length + 1)
	}
	return frameInterval{start: start, end: start + length}
}

// boundedInterval returns a source interval whose rate-scaled output length is
// guaranteed within MaxOutputFrames. It inverts the rounded length-ratio
// relation and then verifies by recomputation, shrinking until the cap holds.
func (g *Generator) boundedInterval(meta generatedSource, targetRate int) frameInterval {
	capFrames := g.config.MaxOutputFrames
	// Largest source length whose rounded scaled length cannot exceed the cap.
	maxLength := int64(float64(capFrames) * float64(meta.rate) / float64(targetRate))
	if maxLength < 1 {
		maxLength = 1
	}
	if maxLength > meta.frames {
		maxLength = meta.frames
	}
	if maxLength > g.config.MaxSourceFrames {
		maxLength = g.config.MaxSourceFrames
	}
	// Recompute defensively because rounding can push the boundary by one.
	for maxLength > 1 && scaledFrames(frameInterval{start: 0, end: maxLength}, meta.rate, targetRate) > capFrames {
		maxLength--
	}
	length := maxLength
	if preferred := g.config.MinSourceFrames; preferred < length {
		// Prefer a shorter, still-meaningful interval when it fits.
		length = preferred + g.rng.Int64N(length-preferred+1)
	}
	start := int64(0)
	if meta.frames > length {
		start = g.rng.Int64N(meta.frames - length + 1)
	}
	return frameInterval{start: start, end: start + length}
}

func (g *Generator) sourceFrames(large bool) int {
	cfg := g.config
	if large {
		// Keep the largest legal fixture inside its own WAV byte cap.
		return int(cfg.MaxSourceFrames)
	}
	span := cfg.MaxSourceFrames - cfg.MinSourceFrames
	return int(cfg.MinSourceFrames + g.rng.Int64N(span/4+1))
}

// synthesize returns float64 PCM for one source plus the frame positions of
// deterministic marker events (impulses). Waveforms 4 and 5 place isolated
// impulses so a renderer that lands a fractional sample on the wrong frame is
// caught by the signal plane even inside a long quiet region.
func (g *Generator) synthesize(frames int, channels int, waveform int, sequence, sourceIndex int64) ([]float64, []int64) {
	rng := g.rng
	samples := make([]float64, frames*channels)
	markers := make([]int64, 0)
	baseFrequency := 110 + rng.Float64()*880
	phase := rng.Float64() * 2 * math.Pi
	for frame := 0; frame < frames; frame++ {
		seconds := float64(frame) / 48000
		var value float64
		switch waveform {
		case 0:
			value = math.Sin(2*math.Pi*baseFrequency*seconds+phase) * 0.5
		case 1:
			// equal-frequency inverted material: crossfades can cancel here.
			value = -math.Sin(2*math.Pi*baseFrequency*seconds+phase) * 0.5
		case 2:
			value = (rng.Float64()*2 - 1) * 0.25
		case 3:
			// mixed harmonics create non-trivial inter-sample structure.
			value = (math.Sin(2*math.Pi*baseFrequency*seconds) +
				0.5*math.Sin(2*math.Pi*baseFrequency*1.5*seconds+phase)) * 0.3
		case 4:
			if frame > 0 && frame%97 == 0 {
				value = 0.9
				markers = append(markers, int64(frame))
			}
		default:
			if frame == frames/3 || frame == (2*frames)/3 {
				value = -0.85
				markers = append(markers, int64(frame))
			} else {
				value = math.Sin(2*math.Pi*baseFrequency*seconds) * 0.15
			}
		}
		for ch := 0; ch < channels; ch++ {
			if channels == 2 {
				samples[frame*2+ch] = value * (1 - 0.25*float64(ch))
			} else {
				samples[frame] = value
			}
		}
	}
	return samples, markers
}

type generatedSource struct {
	rate     int
	channels int
	frames   int64
}

func scaledFrames(interval frameInterval, sourceRate, targetRate int) int64 {
	count := interval.end - interval.start
	return int64(math.Round(float64(count) * float64(targetRate) / float64(sourceRate)))
}

func candidateSourceID(sequence int64, index int) string {
	return sanitizeID("src_" + itoa(sequence) + "_" + itoa(int64(index)))
}

func candidateTimelineID(sequence int64) string {
	return sanitizeID("tl_" + itoa(sequence))
}

func candidateRenderID(sequence int64, trial int64) string {
	return sanitizeID("rnd_" + itoa(sequence) + "_" + itoa(trial))
}

func sanitizeID(value string) string {
	if len(value) > 63 {
		return value[len(value)-63:]
	}
	return value
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [24]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}
