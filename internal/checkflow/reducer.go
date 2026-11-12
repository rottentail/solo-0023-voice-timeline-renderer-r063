package checkflow

import (
	"context"
	"fmt"
	"math"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/oracle"
)

// Reducer shrinks a failing candidate to the smallest sources and timeline that
// still replay the same signal violation. Every shrink attempt is re-executed
// through the real HTTP workflow and independently re-verified; a reduction is
// accepted only when the failure signature (plane:code) is preserved. The
// reducer never widens tolerances and never removes the fractional landing or
// cancellation structure that carries the violation.
type Reducer struct {
	executor  *Executor
	baseSeq   int64
	trial     int64
	attempts  int
	limit     int
	lastProbe ExecutionResult
}

func NewReducer(executor *Executor, baseSequence int64, maxAttempts int) *Reducer {
	return &Reducer{executor: executor, baseSeq: baseSequence, trial: 1, limit: maxAttempts}
}

// Reduce returns a minimized candidate that fails with the same signature as
// the original result, along with its fresh execution result.
func (reducer *Reducer) Reduce(ctx context.Context, failed ExecutionResult) (ExecutionResult, error) {
	signature := failed.FailureCode
	if signature == "" {
		if first, ok := failed.Report.FirstViolation(); ok {
			signature = first.Signature()
		}
	}
	current := cloneForReduction(failed.Candidate, reducer.baseSeq, 0)
	currentResult := failed

	for {
		if reducer.attempts >= reducer.limit {
			break
		}
		changed := false

		// 1. Drop sources that no remaining span references.
		if trimmed := dropUnreferencedSources(current); trimmed != nil {
			result, keep, err := reducer.probe(ctx, *trimmed, signature)
			if err != nil {
				return currentResult, err
			}
			if keep {
				current = *trimmed
				currentResult = result
				changed = true
				continue
			}
		}

		// 2. Classic delta-debugging over the span list.
		reduced, err := reducer.ddminSpans(ctx, current, signature)
		if err != nil {
			return currentResult, err
		}
		if reduced != nil {
			current = *reduced
			currentResult = reducer.lastProbe
			changed = true
			continue
		}

		// 3. Shrink each source interval toward the violation region.
		if shrunk, result, ok, err := reducer.shrinkIntervals(ctx, current, currentResult, signature); err != nil {
			return currentResult, err
		} else if ok {
			current = shrunk
			currentResult = result
			changed = true
			continue
		}

		// 4. Trim retained source WAV payloads to the referenced intervals.
		if trimmed, result, ok, err := reducer.trimSourceWAVs(ctx, current, signature); err != nil {
			return currentResult, err
		} else if ok {
			current = trimmed
			currentResult = result
			changed = true
		}

		if !changed {
			break
		}
	}
	return currentResult, nil
}

// ddminSpans implements Zeller-style delta debugging on the ordered span list,
// repairing crossfade validity after each deletion.
func (reducer *Reducer) ddminSpans(ctx context.Context, candidate GeneratedCandidate, signature string) (*GeneratedCandidate, error) {
	n := 2
	current := candidate
	startingLength := len(current.Spans)
	for n <= len(current.Spans) {
		chunkSize := (len(current.Spans) + n - 1) / n
		acceptedAny := false
		for start := 0; start < len(current.Spans); start += chunkSize {
			end := start + chunkSize
			if end > len(current.Spans) {
				end = len(current.Spans)
			}
			reduced := removeSpanChunk(current, start, end)
			if reduced == nil {
				continue
			}
			result, keep, err := reducer.probe(ctx, *reduced, signature)
			if err != nil {
				return nil, err
			}
			if keep {
				reducer.lastProbe = result
				current = *reduced
				acceptedAny = true
				break
			}
		}
		if acceptedAny {
			n = 2
			continue
		}
		if n >= len(current.Spans) {
			break
		}
		next := n * 2
		if next > len(current.Spans) {
			next = len(current.Spans)
		}
		n = next
	}
	if len(current.Spans) < startingLength {
		return &current, nil
	}
	return nil, nil
}

func (reducer *Reducer) probe(ctx context.Context, candidate GeneratedCandidate, signature string) (ExecutionResult, bool, error) {
	reducer.attempts++
	if reducer.attempts > reducer.limit {
		return ExecutionResult{}, false, nil
	}
	reducer.trial++
	result, err := reducer.executor.Run(ctx, candidate, reducer.baseSeq+int64(reducer.attempts), 1000+reducer.trial)
	if err != nil {
		return ExecutionResult{}, false, err
	}
	if !result.Failed() {
		return result, false, nil
	}
	observed := result.FailureCode
	if observed == "" {
		if first, ok := result.Report.FirstViolation(); ok {
			observed = first.Signature()
		}
	}
	return result, observed == signature, nil
}

// removeSpanChunk deletes spans [start,end) and revalidates the remaining
// neighbor relations: the new first span loses its crossfade and every later
// crossfade is clamped to both neighboring output lengths.
func removeSpanChunk(candidate GeneratedCandidate, start, end int) *GeneratedCandidate {
	if start >= end || end > len(candidate.Spans) || len(candidate.Spans) == 1 {
		return nil
	}
	spans := append([]domain.TimelineSpan(nil), candidate.Spans[:start]...)
	spans = append(spans, candidate.Spans[end:]...)
	oracleSpans := make([]oracle.Span, 0, len(spans))
	for _, span := range spans {
		oracleSpans = append(oracleSpans, oracle.Span{
			SourceID:         span.SourceID,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			GainDB:           span.GainDB,
			CrossfadeFrames:  span.CrossfadeFrames,
		})
	}
	repairCrossfades(spans, oracleSpans, candidate)
	if !spansLookLegal(spans, candidate) {
		return nil
	}
	clone := cloneShapes(candidate)
	clone.Spans = spans
	clone.Oracle.Timeline.Spans = oracleSpans
	clone.OutputFrames = projectOutputFrames(clone)
	return &clone
}

// shrinkIntervals narrows source intervals while keeping the output region
// around any violation frame, so fractional landing points survive reduction.
func (reducer *Reducer) shrinkIntervals(
	ctx context.Context,
	candidate GeneratedCandidate,
	result ExecutionResult,
	signature string,
) (GeneratedCandidate, ExecutionResult, bool, error) {
	violationFrame := int64(-1)
	for _, violation := range result.Report.Violations {
		if violation.Frame >= 0 {
			violationFrame = violation.Frame
			break
		}
	}
	for index := range candidate.Spans {
		working := candidate
		for attempt := 0; attempt < 6; attempt++ {
			shrunk := cloneShapes(working)
			shrunk.Spans = append([]domain.TimelineSpan(nil), working.Spans...)
			shrunk.Oracle.Timeline.Spans = append([]oracle.Span(nil), working.Oracle.Timeline.Spans...)
			span := shrunk.Spans[index]
			meta := working.SourceMeta[span.SourceID]
			currentLength := span.SourceEndFrame - span.SourceStartFrame
			if currentLength <= 1 {
				break
			}
			newLength := currentLength / 2
			if newLength < 1 {
				newLength = 1
			}
			newStart := span.SourceStartFrame
			newEnd := newStart + newLength
			// Keep the interval covering the source-side region that maps to
			// the violating output frame when one is known.
			if violationFrame >= 0 {
				outputStart := spanOutputStart(shrunk, index)
				outputEnd := outputStart + scaledFrames(frameInterval{start: newStart, end: newEnd}, meta.rate, candidate.SampleRate)
				if violationFrame < outputStart || violationFrame >= outputEnd {
					newStart = span.SourceEndFrame - newLength
					newEnd = span.SourceEndFrame
				}
			}
			if newStart < 0 || newEnd > meta.frames || newEnd <= newStart {
				break
			}
			shrunk.Spans[index].SourceStartFrame = newStart
			shrunk.Spans[index].SourceEndFrame = newEnd
			shrunk.Oracle.Timeline.Spans[index].SourceStartFrame = newStart
			shrunk.Oracle.Timeline.Spans[index].SourceEndFrame = newEnd
			repairCrossfades(shrunk.Spans, shrunk.Oracle.Timeline.Spans, shrunk)
			if !spansLookLegal(shrunk.Spans, shrunk) {
				break
			}
			shrunk.OutputFrames = projectOutputFrames(shrunk)
			probe, keep, err := reducer.probe(ctx, shrunk, signature)
			if err != nil {
				return candidate, result, false, err
			}
			if keep {
				working = shrunk
				result = probe
				candidate = shrunk
				return candidate, result, true, nil
			}
		}
	}
	return candidate, result, false, nil
}

// trimSourceWAVs re-encodes each source so it contains only the frames the
// reduced timeline references and shifts intervals to the new local origin.
func (reducer *Reducer) trimSourceWAVs(ctx context.Context, candidate GeneratedCandidate, signature string) (GeneratedCandidate, ExecutionResult, bool, error) {
	trimmed := cloneShapes(candidate)
	trimmed.Spans = append([]domain.TimelineSpan(nil), candidate.Spans...)
	trimmed.Oracle.Timeline.Spans = append([]oracle.Span(nil), candidate.Oracle.Timeline.Spans...)
	trimmed.SourceWAVs = make(map[string][]byte, len(candidate.SourceWAVs))
	trimmed.SourceMeta = make(map[string]generatedSource, len(candidate.SourceMeta))
	trimmed.Oracle.Sources = make([]oracle.Source, 0, len(candidate.Oracle.Sources))

	offsets := make(map[string]int64)
	for id, wav := range candidate.SourceWAVs {
		minFrame, maxFrame := int64(math.MaxInt64), int64(0)
		referenced := false
		for _, span := range trimmed.Spans {
			if span.SourceID != id {
				continue
			}
			referenced = true
			if span.SourceStartFrame < minFrame {
				minFrame = span.SourceStartFrame
			}
			if span.SourceEndFrame > maxFrame {
				maxFrame = span.SourceEndFrame
			}
		}
		if !referenced {
			continue
		}
		parsed, err := oracle.ParseWAV(wav)
		if err != nil {
			return candidate, ExecutionResult{}, false, nil
		}
		localSamples := make([]float64, (maxFrame-minFrame)*int64(parsed.Channels))
		copy(localSamples, parsed.Samples[minFrame*int64(parsed.Channels):maxFrame*int64(parsed.Channels)])
		smaller, err := oracle.EncodeWAV(parsed.SampleRate, parsed.Channels, localSamples)
		if err != nil {
			return candidate, ExecutionResult{}, false, nil
		}
		if len(smaller) >= len(wav) {
			trimmed.SourceWAVs[id] = wav
			trimmed.SourceMeta[id] = candidate.SourceMeta[id]
			trimmed.Oracle.Sources = append(trimmed.Oracle.Sources, oracle.Source{ID: id, WAV: wav})
			continue
		}
		offsets[id] = minFrame
		trimmed.SourceWAVs[id] = smaller
		trimmed.SourceMeta[id] = generatedSource{
			rate:     parsed.SampleRate,
			channels: parsed.Channels,
			frames:   maxFrame - minFrame,
		}
		trimmed.Oracle.Sources = append(trimmed.Oracle.Sources, oracle.Source{ID: id, WAV: smaller})
	}
	for index := range trimmed.Spans {
		if offset, ok := offsets[trimmed.Spans[index].SourceID]; ok {
			trimmed.Spans[index].SourceStartFrame -= offset
			trimmed.Spans[index].SourceEndFrame -= offset
			trimmed.Oracle.Timeline.Spans[index].SourceStartFrame -= offset
			trimmed.Oracle.Timeline.Spans[index].SourceEndFrame -= offset
		}
	}
	if !spansLookLegal(trimmed.Spans, trimmed) {
		return candidate, ExecutionResult{}, false, nil
	}
	trimmed.OutputFrames = projectOutputFrames(trimmed)
	probe, keep, err := reducer.probe(ctx, trimmed, signature)
	if err != nil || !keep {
		return candidate, ExecutionResult{}, false, err
	}
	return trimmed, probe, true, nil
}

func dropUnreferencedSources(candidate GeneratedCandidate) *GeneratedCandidate {
	referenced := make(map[string]bool)
	for _, span := range candidate.Spans {
		referenced[span.SourceID] = true
	}
	if len(referenced) == len(candidate.SourceWAVs) {
		return nil
	}
	trimmed := cloneShapes(candidate)
	trimmed.SourceWAVs = make(map[string][]byte, len(referenced))
	trimmed.SourceMeta = make(map[string]generatedSource, len(referenced))
	trimmed.Oracle.Sources = make([]oracle.Source, 0, len(referenced))
	for id, wav := range candidate.SourceWAVs {
		if referenced[id] {
			trimmed.SourceWAVs[id] = wav
			trimmed.SourceMeta[id] = candidate.SourceMeta[id]
		}
	}
	for _, source := range candidate.Oracle.Sources {
		if referenced[source.ID] {
			trimmed.Oracle.Sources = append(trimmed.Oracle.Sources, source)
		}
	}
	return &trimmed
}

func repairCrossfades(spans []domain.TimelineSpan, oracleSpans []oracle.Span, candidate GeneratedCandidate) {
	for index := range spans {
		if index == 0 {
			spans[index].CrossfadeFrames = 0
			oracleSpans[index].CrossfadeFrames = 0
			continue
		}
		meta := candidate.SourceMeta[spans[index].SourceID]
		frames := scaledFrames(
			frameInterval{start: spans[index].SourceStartFrame, end: spans[index].SourceEndFrame},
			meta.rate, candidate.SampleRate,
		)
		prevMeta := candidate.SourceMeta[spans[index-1].SourceID]
		prevFrames := scaledFrames(
			frameInterval{start: spans[index-1].SourceStartFrame, end: spans[index-1].SourceEndFrame},
			prevMeta.rate, candidate.SampleRate,
		)
		limit := frames
		if prevFrames < limit {
			limit = prevFrames
		}
		if spans[index].CrossfadeFrames > limit {
			spans[index].CrossfadeFrames = limit
			oracleSpans[index].CrossfadeFrames = limit
		}
	}
}

func spansLookLegal(spans []domain.TimelineSpan, candidate GeneratedCandidate) bool {
	if len(spans) == 0 {
		return false
	}
	for index, span := range spans {
		meta, ok := candidate.SourceMeta[span.SourceID]
		if !ok {
			return false
		}
		if span.SourceStartFrame < 0 || span.SourceEndFrame <= span.SourceStartFrame || span.SourceEndFrame > meta.frames {
			return false
		}
		if math.IsNaN(span.GainDB) || span.GainDB < -36 || span.GainDB > 12 {
			return false
		}
		if index == 0 && span.CrossfadeFrames != 0 {
			return false
		}
		if span.CrossfadeFrames < 0 {
			return false
		}
	}
	return projectOutputFrames(candidate) > 0
}

func projectOutputFrames(candidate GeneratedCandidate) int64 {
	total := int64(0)
	for index, span := range candidate.Spans {
		meta := candidate.SourceMeta[span.SourceID]
		total += scaledFrames(frameInterval{start: span.SourceStartFrame, end: span.SourceEndFrame}, meta.rate, candidate.SampleRate)
		if index > 0 {
			total -= span.CrossfadeFrames
		}
	}
	return total
}

func spanOutputStart(candidate GeneratedCandidate, index int) int64 {
	cursor := int64(0)
	for i := 0; i < index; i++ {
		meta := candidate.SourceMeta[candidate.Spans[i].SourceID]
		cursor += scaledFrames(
			frameInterval{start: candidate.Spans[i].SourceStartFrame, end: candidate.Spans[i].SourceEndFrame},
			meta.rate, candidate.SampleRate,
		)
		cursor -= candidate.Spans[i+1].CrossfadeFrames
	}
	return cursor
}

func cloneShapes(candidate GeneratedCandidate) GeneratedCandidate {
	clone := candidate
	clone.Spans = append([]domain.TimelineSpan(nil), candidate.Spans...)
	clone.Oracle.Timeline.Spans = append([]oracle.Span(nil), candidate.Oracle.Timeline.Spans...)
	clone.Oracle.Sources = append([]oracle.Source(nil), candidate.Oracle.Sources...)
	return clone
}

// cloneForReduction gives each reduction trial globally unique identifiers so
// parallel reducers can never collide on a source, timeline, or render id.
func cloneForReduction(candidate GeneratedCandidate, baseSeq, trial int64) GeneratedCandidate {
	clone := cloneShapes(candidate)
	clone.Oracle.Timeline.ID = fmt.Sprintf("tl_red_%d_%d", baseSeq, trial)
	rewrite := make(map[string]string)
	for index, source := range clone.Oracle.Sources {
		newID := fmt.Sprintf("%s_r%d_%d", source.ID, baseSeq, trial)
		rewrite[source.ID] = newID
		clone.Oracle.Sources[index].ID = newID
	}
	newWAVs := make(map[string][]byte, len(clone.SourceWAVs))
	newMeta := make(map[string]generatedSource, len(clone.SourceMeta))
	for oldID, newID := range rewrite {
		newWAVs[newID] = clone.SourceWAVs[oldID]
		newMeta[newID] = clone.SourceMeta[oldID]
	}
	clone.SourceWAVs = newWAVs
	clone.SourceMeta = newMeta
	for index := range clone.Spans {
		clone.Spans[index].SourceID = rewrite[clone.Spans[index].SourceID]
		clone.Oracle.Timeline.Spans[index].SourceID = rewrite[clone.Spans[index].SourceID]
	}
	return clone
}
