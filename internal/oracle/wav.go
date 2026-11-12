package oracle

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// PCM16 is an independently parsed 16-bit PCM WAVE payload. Quantized holds
// the raw signed integers exactly as stored; Samples holds the dequantized
// floating-point values derived with the asymmetric full-scale convention.
type PCM16 struct {
	SampleRate int
	Channels   int
	FrameCount int64
	Quantized  []int16
	Samples    []float64
}

const (
	pcm16MaxPositive = 32767.0
	pcm16MinNegative = 32768.0
)

// dequantize applies the asymmetric PCM16 scale used by the format under test:
// positive integers divide by 32767, negative magnitudes by 32768.
func dequantize(raw int16) float64 {
	if raw < 0 {
		return float64(raw) / pcm16MinNegative
	}
	return float64(raw) / pcm16MaxPositive
}

// ParseWAV decodes a RIFF/WAVE PCM16 file using only this package's own
// parser. It intentionally does not call the production codec so a shared
// parser bug cannot mask a renderer disagreement.
func ParseWAV(data []byte) (*PCM16, error) {
	if len(data) < 44 {
		return nil, errors.New("payload shorter than a canonical PCM WAVE header")
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, errors.New("missing RIFF/WAVE signature")
	}
	riffSize := binary.LittleEndian.Uint32(data[4:8])
	if uint64(riffSize)+8 > uint64(len(data)) {
		return nil, errors.New("RIFF size overruns the payload")
	}

	var (
		audioFormat   uint16
		channels      uint16
		sampleRate    uint32
		bitsPerSample uint16
		dataBytes     []byte
	)
	offset := 12
	for offset+8 <= len(data) {
		chunkID := string(data[offset : offset+4])
		chunkSize := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		start := offset + 8
		end := start + chunkSize
		if end < start || end > len(data) {
			return nil, fmt.Errorf("chunk %q overruns the payload", chunkID)
		}
		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return nil, errors.New("fmt chunk is too short")
			}
			audioFormat = binary.LittleEndian.Uint16(data[start : start+2])
			channels = binary.LittleEndian.Uint16(data[start+2 : start+4])
			sampleRate = binary.LittleEndian.Uint32(data[start+4 : start+8])
			bitsPerSample = binary.LittleEndian.Uint16(data[start+14 : start+16])
		case "data":
			dataBytes = data[start:end]
		}
		offset = end
		if chunkSize%2 == 1 {
			offset++
		}
	}
	if audioFormat != 1 {
		return nil, fmt.Errorf("unsupported format tag %d", audioFormat)
	}
	if bitsPerSample != 16 {
		return nil, fmt.Errorf("unsupported bit width %d", bitsPerSample)
	}
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("unsupported channel count %d", channels)
	}
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, fmt.Errorf("sample rate %d outside accepted range", sampleRate)
	}
	if len(dataBytes) == 0 || len(dataBytes)%2 != 0 || len(dataBytes)%int(channels*2) != 0 {
		return nil, errors.New("data chunk is empty or not frame aligned")
	}
	count := len(dataBytes) / 2
	quantized := make([]int16, count)
	samples := make([]float64, count)
	for index := 0; index < count; index++ {
		raw := int16(binary.LittleEndian.Uint16(dataBytes[index*2 : index*2+2]))
		quantized[index] = raw
		samples[index] = dequantize(raw)
	}
	return &PCM16{
		SampleRate: int(sampleRate),
		Channels:   int(channels),
		FrameCount: int64(count / int(channels)),
		Quantized:  quantized,
		Samples:    samples,
	}, nil
}

// EncodeWAV writes a canonical 44-byte-header PCM16 WAVE from float64
// samples. It is used for synthesizing oracle-side test inputs and is an
// independent implementation of the container writer.
func EncodeWAV(sampleRate, channels int, samples []float64) ([]byte, error) {
	if sampleRate < 8000 || sampleRate > 192000 {
		return nil, errors.New("sample rate outside accepted range")
	}
	if channels != 1 && channels != 2 {
		return nil, errors.New("channel count must be one or two")
	}
	if len(samples) == 0 || len(samples)%channels != 0 {
		return nil, errors.New("samples are empty or not frame aligned")
	}
	dataLen := len(samples) * 2
	total := 44 + dataLen
	buf := make([]byte, total)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(total-8))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sampleRate*channels*2))
	binary.LittleEndian.PutUint16(buf[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(dataLen))
	for index, sample := range samples {
		q := QuantizeOne(sample)
		binary.LittleEndian.PutUint16(buf[44+index*2:46+index*2], uint16(q))
	}
	return buf, nil
}

// QuantizeOne returns the nearest PCM16 integer for a normalized sample,
// clamping at the asymmetric rails and rounding half away from zero.
func QuantizeOne(sample float64) int16 {
	var q int64
	switch {
	case sample <= -1:
		return -32768
	case sample >= 1:
		return 32767
	case sample < 0:
		q = int64(math.Round(sample * pcm16MinNegative))
	default:
		q = int64(math.Round(sample * pcm16MaxPositive))
	}
	if q < -32768 {
		q = -32768
	}
	if q > 32767 {
		q = 32767
	}
	return int16(q)
}
