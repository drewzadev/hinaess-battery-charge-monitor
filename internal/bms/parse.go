// This file decodes the analog-data INFO payload into a PackSample. It is a
// faithful port of parse_analog_response (src/serial_reader.py:197-320),
// following the layout table in the PRD's Current State section. Every read is
// bounds-checked (the reference relies on forgiving Python slicing); a short
// payload yields ErrFraming rather than a slice panic. After decode a
// plausibility check (Goals §3) nulls out-of-range secondary fields and rejects
// the whole sample with ErrImplausible when a core voltage is implausible.
package bms

import (
	"fmt"
	"log/slog"
)

// tempLabels are the probe labels by index, lowercased per Q3 (Option A) to
// match Slice 2's temp_samples.probe schema. Source order: src/config.py:54.
var tempLabels = []string{"t1", "t2", "t3", "t4", "mos", "env"}

// Plausibility bounds for decoded values (Goals §3 / the PRD's [refine] note).
// Values outside these ranges are nulled (temps, current, SOC/SOH) or cause
// ErrImplausible (core cell/pack voltages) so the caller discards a garbage
// sample rather than reporting it. Tune to the actual pack before relying on
// these in production.
const (
	cellMVMin    = 1000  // per-cell voltage floor, mV
	cellMVMax    = 5000  // per-cell voltage ceiling, mV
	packMVMin    = 10000 // pack voltage floor, mV (16S)
	packMVMax    = 60000 // pack voltage ceiling, mV (16S)
	currentMAMax = 500000 // |current| ceiling, mA
	deciCMin     = -500  // temperature floor, deci-°C (−50 °C)
	deciCMax     = 1500  // temperature ceiling, deci-°C (150 °C)

	tempOffset = 2731 // 0 °C in deci-kelvin; deci-°C = raw − 2731
)

// ParseAnalog decodes an analog-data INFO payload (the bytes from a parsed
// frame) into a PackSample. The Timestamp is left zero for the caller to stamp
// at receive time. Returns ErrFraming on a truncated payload and ErrImplausible
// when a core value (cell or pack voltage) falls outside its sane range.
func ParseAnalog(info []byte) (PackSample, error) {
	// Coarse minimum, mirroring the reference's len(data) < 10 guard
	// (src/serial_reader.py:226-227); per-read checks below catch finer truncation.
	if len(info) < 10 {
		return PackSample{}, fmt.Errorf("%w: INFO too short (%d bytes)", ErrFraming, len(info))
	}

	var s PackSample
	p := 0
	// need reports whether n more bytes remain from the cursor p.
	need := func(n int) bool { return p+n <= len(info) }
	// u16 reads a big-endian unsigned 16-bit value at the cursor.
	u16 := func() int { return int(info[p])<<8 | int(info[p+1]) }

	// [0] flag / address echo (ignored), [1] cell count.
	p++ // flag
	cellCount := int(info[p])
	p++
	if cellCount > 32 {
		cellCount = 32
	}

	// ── Cell voltages: raw u16 BE is already millivolts (src/serial_reader.py:246). ──
	s.Cells = make([]int, 0, cellCount)
	for i := 0; i < cellCount; i++ {
		if !need(2) {
			return PackSample{}, fmt.Errorf("%w: INFO truncated reading cell %d", ErrFraming, i+1)
		}
		s.Cells = append(s.Cells, u16())
		p += 2
	}

	// ── Temperatures: deci-°C = raw − 2731 (src/serial_reader.py:275). ──
	if need(1) {
		tempCount := int(info[p])
		p++
		if tempCount > 8 {
			tempCount = 8
		}
		for i := 0; i < tempCount; i++ {
			if !need(2) {
				return PackSample{}, fmt.Errorf("%w: INFO truncated reading temp %d", ErrFraming, i+1)
			}
			deci := u16() - tempOffset
			p += 2
			// Null out-of-range probes as the reference does
			// (src/serial_reader.py:276-277): omit them from the slice.
			if deci < deciCMin || deci > deciCMax {
				continue
			}
			label := fmt.Sprintf("t%d", i+1)
			if i < len(tempLabels) {
				label = tempLabels[i]
			}
			s.Temps = append(s.Temps, Temp{Probe: label, DeciC: deci})
		}
	}

	// ── Main values: current (s16 ×10 mA), pack voltage and remaining capacity
	// (u16 ×10 mV / mAh). The raw unit is 10 mV / 10 mA / 10 mAh
	// (src/serial_reader.py:286-298). ──
	if need(6) {
		raw := u16()
		if raw > 32767 { // two's-complement s16; + charge, − discharge
			raw -= 65536
		}
		s.PackMA = raw * 10
		p += 2
		s.PackMV = u16() * 10
		p += 2
		s.RemainMAH = u16() * 10
		p += 2

		// Skip byte (observed 0x05), then full capacity (src/serial_reader.py:295-299).
		if p+3 <= len(info) {
			p++ // skip byte
			s.FullMAH = u16() * 10
			p += 2

			// Skip byte (observed 0x00), then three u8 fields:
			// cycle count, soc_int, SOH (src/serial_reader.py:306-315).
			if p+4 <= len(info) {
				p++ // skip byte
				s.Cycles = int(info[p])
				p++
				socInt := int(info[p]) // raw u8 SOC per Q2 (Option B), not remain/full
				p++
				soh := int(info[p])
				p++
				s.SOCPct = float64(socInt)
				s.SOHPct = float64(soh)
			}
		}
	}

	// ── Plausibility check (Goals §3). Core voltages out of range discard the
	// whole sample; secondary fields out of range are nulled. ──
	for i, mv := range s.Cells {
		if mv < cellMVMin || mv > cellMVMax {
			return PackSample{}, fmt.Errorf("%w: cell %d voltage %d mV out of range [%d,%d]",
				ErrImplausible, i+1, mv, cellMVMin, cellMVMax)
		}
	}
	if s.PackMV < packMVMin || s.PackMV > packMVMax {
		return PackSample{}, fmt.Errorf("%w: pack voltage %d mV out of range [%d,%d]",
			ErrImplausible, s.PackMV, packMVMin, packMVMax)
	}
	if s.PackMA < -currentMAMax || s.PackMA > currentMAMax {
		slog.Warn("bms: implausible current nulled", "pack_ma", s.PackMA)
		s.PackMA = 0
	}
	if s.SOCPct < 0 || s.SOCPct > 100 {
		slog.Warn("bms: implausible SOC nulled", "soc_pct", s.SOCPct)
		s.SOCPct = 0
	}
	if s.SOHPct < 0 || s.SOHPct > 100 {
		slog.Warn("bms: implausible SOH nulled", "soh_pct", s.SOHPct)
		s.SOHPct = 0
	}

	return s, nil
}
