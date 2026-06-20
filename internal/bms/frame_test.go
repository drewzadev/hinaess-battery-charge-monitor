package bms

import (
	"bytes"
	"errors"
	"testing"
)

// buildResponse assembles a syntactically valid response frame from an inner
// hex body (ver..INFO) by appending the correctly computed checksum, mirroring
// how the BMS frames a reply. The header begins with ver=21, addr=00, cid1=46,
// cid2=00 (RTN OK) so parseFrame accepts it.
func buildResponse(inner string) string {
	return inner + checksum(inner)
}

// AC-1: the checksum must sum the ASCII code points of the hex string, giving
// the reference's verified value (src/main.py:44).
func TestChecksum(t *testing.T) {
	if got := checksum("21004642E00200"); got != "FD36" {
		t.Errorf("checksum(\"21004642E00200\") = %q; want %q", got, "FD36")
	}
}

// AC-2: BuildAnalogRequest(0) must reproduce the reference's verified wire frame
// byte-for-byte (src/serial_reader.py:43-44, src/main.py:44).
func TestBuildAnalogRequest(t *testing.T) {
	want := []byte("~21004642E00200FD36\r")
	got := BuildAnalogRequest(0)
	if !bytes.Equal(got, want) {
		t.Errorf("BuildAnalogRequest(0) = %q; want %q", got, want)
	}
}

// AC-4(a): a checksum mismatch must be non-fatal — parseFrame still succeeds and
// extracts the INFO payload, but reports ChecksumOK == false.
func TestParseFrameChecksumNonFatal(t *testing.T) {
	// ver=21 addr=00 cid1=46 cid2=00 length=C004 (info_hex_len 4) INFO=0110
	inner := "21004600C0040110"
	good := buildResponse(inner)

	f, err := parseFrame(good)
	if err != nil {
		t.Fatalf("parseFrame(good) returned error: %v", err)
	}
	if !f.ChecksumOK {
		t.Errorf("ChecksumOK = false for a correctly checksummed frame; want true")
	}
	if len(f.Info) != 2 {
		t.Fatalf("len(Info) = %d; want 2", len(f.Info))
	}

	// Flip one nibble of the trailing checksum field; the body is unchanged.
	corrupted := []byte(good)
	last := len(corrupted) - 1
	if corrupted[last] == 'A' {
		corrupted[last] = 'B'
	} else {
		corrupted[last] = 'A'
	}

	f2, err := parseFrame(string(corrupted))
	if err != nil {
		t.Fatalf("checksum mismatch must be non-fatal, but parseFrame returned: %v", err)
	}
	if f2.ChecksumOK {
		t.Errorf("ChecksumOK = true for a corrupted checksum; want false")
	}
	if len(f2.Info) != 2 || f2.Info[0] != 0x01 || f2.Info[1] != 0x10 {
		t.Errorf("INFO not decoded despite bad checksum: got % X", f2.Info)
	}
}

// AC-5: a truncated INFO (length field declares more hex chars than present)
// returns ErrFraming with no panic.
func TestParseFrameTruncatedInfo(t *testing.T) {
	// length=E010 → info_hex_len 0x010 = 16 chars declared, but only 4 present.
	truncated := "21004600E0100102"
	_, err := parseFrame(truncated)
	if !errors.Is(err, ErrFraming) {
		t.Fatalf("parseFrame(truncated) error = %v; want ErrFraming", err)
	}
}

func TestParseFrameTooShort(t *testing.T) {
	_, err := parseFrame("21004600")
	if !errors.Is(err, ErrFraming) {
		t.Fatalf("parseFrame(short) error = %v; want ErrFraming", err)
	}
}

func TestParseFrameOddInfoLength(t *testing.T) {
	// length=E003 → info_hex_len 3 (odd) → ErrFraming.
	_, err := parseFrame("21004600E0030011")
	if !errors.Is(err, ErrFraming) {
		t.Fatalf("parseFrame(odd) error = %v; want ErrFraming", err)
	}
}

func TestParseFrameNonZeroRTN(t *testing.T) {
	// cid2=09 (non-zero RTN) with an otherwise well-formed frame → ErrBMS.
	inner := "21004609C0040110"
	_, err := parseFrame(buildResponse(inner))
	if !errors.Is(err, ErrBMS) {
		t.Fatalf("parseFrame(rtn!=0) error = %v; want ErrBMS", err)
	}
}
