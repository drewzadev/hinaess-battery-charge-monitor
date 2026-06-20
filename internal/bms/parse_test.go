package bms

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadAnalogFrame reads the committed analog-response fixture
// (testdata/analog_frame.hex) and returns the cleaned hex frame parseFrame
// consumes. Lines beginning with '#' and surrounding whitespace are stripped, so
// the fixture file can carry its own provenance notes inline. This frame is the
// Q5 Option-(b) stopgap (synthetic, correctly checksummed) until a real capture
// from the live pack replaces it — see the header of the fixture file.
func loadAnalogFrame(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "analog_frame.hex"))
	if err != nil {
		t.Fatalf("reading analog frame fixture: %v", err)
	}
	var sb strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sb.WriteString(line)
	}
	hexFrame := strings.ToUpper(sb.String())
	if hexFrame == "" {
		t.Fatal("analog frame fixture is empty after stripping comments")
	}
	return hexFrame
}

// loadAnalogInfo decodes the fixture through parseFrame and returns its INFO
// payload bytes, so the plausibility tests can mutate a copy at known offsets.
func loadAnalogInfo(t *testing.T) []byte {
	t.Helper()
	f, err := parseFrame(loadAnalogFrame(t))
	if err != nil {
		t.Fatalf("parseFrame(fixture) returned error: %v", err)
	}
	return f.Info
}

// AC-3: the committed analog frame decodes through parseFrame → ParseAnalog to
// 16 cells, a plausible pack mV, a correctly-signed current, and labeled temps.
//
// The expected values below describe the synthetic fixture; when a real frame is
// captured (Q5 Option A) replace testdata/analog_frame.hex and update these to
// the reference's decoded values.
func TestParseAnalogDecode(t *testing.T) {
	f, err := parseFrame(loadAnalogFrame(t))
	if err != nil {
		t.Fatalf("parseFrame returned error: %v", err)
	}
	if !f.ChecksumOK {
		t.Errorf("ChecksumOK = false for the committed fixture; its checksum should validate")
	}
	s, err := ParseAnalog(f.Info)
	if err != nil {
		t.Fatalf("ParseAnalog returned error: %v", err)
	}

	if len(s.Cells) != 16 {
		t.Fatalf("len(Cells) = %d; want 16", len(s.Cells))
	}
	for i, mv := range s.Cells {
		if mv != 3300 {
			t.Errorf("Cells[%d] = %d mV; want 3300", i, mv)
		}
	}
	if s.PackMV != 52800 {
		t.Errorf("PackMV = %d; want 52800", s.PackMV)
	}
	if s.PackMA != -10000 {
		t.Errorf("PackMA = %d; want -10000 (signed discharge)", s.PackMA)
	}
	if s.RemainMAH != 100000 {
		t.Errorf("RemainMAH = %d; want 100000", s.RemainMAH)
	}
	if s.FullMAH != 200000 {
		t.Errorf("FullMAH = %d; want 200000", s.FullMAH)
	}
	if s.Cycles != 42 {
		t.Errorf("Cycles = %d; want 42", s.Cycles)
	}
	if s.SOCPct != 75 {
		t.Errorf("SOCPct = %g; want 75 (raw u8 soc_int, Q2)", s.SOCPct)
	}
	if s.SOHPct != 98 {
		t.Errorf("SOHPct = %g; want 98", s.SOHPct)
	}

	if len(s.Temps) != 2 {
		t.Fatalf("len(Temps) = %d; want 2", len(s.Temps))
	}
	if s.Temps[0].Probe != "t1" || s.Temps[1].Probe != "t2" {
		t.Errorf("temp probes = %q,%q; want lowercased t1,t2", s.Temps[0].Probe, s.Temps[1].Probe)
	}
	if s.Temps[0].DeciC != 251 {
		t.Errorf("Temps[0].DeciC = %d; want 251 (25.1 °C)", s.Temps[0].DeciC)
	}
}

// AC-4(a): flipping one checksum nibble of the captured frame still decodes to a
// full sample — parseFrame reports ChecksumOK == false (and logs a warning), but
// because the decoded values stay plausible ParseAnalog still yields 16 cells.
func TestParseAnalogChecksumNonFatalStillDecodes(t *testing.T) {
	frame := []byte(loadAnalogFrame(t))

	// Flip the last checksum nibble; the body (ver..INFO) is unchanged.
	last := len(frame) - 1
	if frame[last] == 'A' {
		frame[last] = 'B'
	} else {
		frame[last] = 'A'
	}

	f, err := parseFrame(string(frame))
	if err != nil {
		t.Fatalf("checksum mismatch must be non-fatal, but parseFrame returned: %v", err)
	}
	if f.ChecksumOK {
		t.Errorf("ChecksumOK = true for a corrupted checksum; want false")
	}

	s, err := ParseAnalog(f.Info)
	if err != nil {
		t.Fatalf("ParseAnalog returned error despite plausible values: %v", err)
	}
	if len(s.Cells) != 16 {
		t.Errorf("len(Cells) = %d; want 16 (full sample despite bad checksum)", len(s.Cells))
	}
}

// AC-4(b), part 1: an out-of-range temperature is nulled (omitted from Temps)
// rather than rejecting the whole sample.
func TestParseAnalogNullsOutOfRangeTemp(t *testing.T) {
	info := loadAnalogInfo(t)
	// The first temperature lives at offset 1(flag)+1(count)+32(cells)+1(tempcount) = 35.
	// Set its raw to 0 → deci-°C = −2731, well outside −50..150 °C.
	tempOff := 1 + 1 + 16*2 + 1
	info[tempOff] = 0x00
	info[tempOff+1] = 0x00

	s, err := ParseAnalog(info)
	if err != nil {
		t.Fatalf("ParseAnalog returned error: %v", err)
	}
	if len(s.Temps) != 1 {
		t.Fatalf("len(Temps) = %d; want 1 (out-of-range temp nulled)", len(s.Temps))
	}
	// The surviving probe is t2 — labels are index-based, so nulling t1 keeps t2's label.
	if s.Temps[0].Probe != "t2" {
		t.Errorf("surviving probe = %q; want t2", s.Temps[0].Probe)
	}
}

// AC-4(b), part 2: corrupting a core value (a cell voltage) out of range rejects
// the whole sample with ErrImplausible.
func TestParseAnalogImplausibleCell(t *testing.T) {
	info := loadAnalogInfo(t)
	// First cell starts at offset 2; set it to 0 mV (below the 1000 mV floor).
	info[2] = 0x00
	info[3] = 0x00

	_, err := ParseAnalog(info)
	if !errors.Is(err, ErrImplausible) {
		t.Fatalf("ParseAnalog(corrupted cell) error = %v; want ErrImplausible", err)
	}
}

// AC-4(b), part 2: an implausible pack voltage likewise rejects the sample.
func TestParseAnalogImplausiblePackVoltage(t *testing.T) {
	info := loadAnalogInfo(t)
	// Pack voltage follows flag(1)+count(1)+cells(32)+tempcount(1)+temps(2×2)+current(2) = offset 41.
	// Set its raw to 0 → 0 mV (below the 10000 mV floor).
	packOff := 1 + 1 + 16*2 + 1 + 2*2 + 2
	info[packOff] = 0x00
	info[packOff+1] = 0x00

	_, err := ParseAnalog(info)
	if !errors.Is(err, ErrImplausible) {
		t.Fatalf("ParseAnalog(low pack voltage) error = %v; want ErrImplausible", err)
	}
}

// A truncated INFO payload returns ErrFraming with no panic (bounds-checked reads).
func TestParseAnalogShortPayload(t *testing.T) {
	info := loadAnalogInfo(t)[:20] // chop mid-cell-array
	_, err := ParseAnalog(info)
	if !errors.Is(err, ErrFraming) {
		t.Fatalf("ParseAnalog(truncated) error = %v; want ErrFraming", err)
	}
}
