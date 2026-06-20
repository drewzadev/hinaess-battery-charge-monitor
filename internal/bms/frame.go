// Package bms implements the PACE/Topband ASCII-hex-framed serial protocol
// used by the HinaEss Powergem Max BMS. This file covers frame construction:
// the protocol constants, the checksum and length-field encoders, and the
// analog-data request builder. It is a faithful port of the reference
// implementation in /opt/ha-hinaess-powergem/src/serial_reader.py.
package bms

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// Protocol constants (src/serial_reader.py:37, src/config.py:21-30).
const (
	SOI = '~'  // start of information, 0x7E
	EOI = '\r' // end of information, 0x0D

	Ver        = 0x21 // protocol version byte
	CID1       = 0x46 // device type identifier
	CID2Analog = 0x42 // command: get analog data
	CID2Proto  = 0x4F // command: get protocol version (used as a wake-up ping)
	RTNOk      = 0x00 // return code: success
)

// checksum sums the ASCII code points of each character in innerHex (the frame
// body between SOI and the checksum field), then returns (~total + 1) & 0xFFFF
// formatted as four uppercase hex characters. The sum is over the ASCII text
// characters, not the decoded bytes — e.g. checksum("21004642E00200") == "FD36"
// (src/serial_reader.py:20-23).
func checksum(innerHex string) string {
	var total int
	for _, c := range innerHex {
		total += int(c)
	}
	sum := (^total + 1) & 0xFFFF
	return fmt.Sprintf("%04X", sum)
}

// encodeLength encodes the LENGTH field for an INFO payload of hexCharCount
// ASCII-hex characters: the low 12 bits hold the count and the high 4 bits hold
// its checksum nibble, (~(count & 0xFFF) + 1) & 0xF. For an INFO of "00"
// (count 2) the field is 0xE002 → "E002" (src/serial_reader.py:26-29).
func encodeLength(hexCharCount int) string {
	lchksum := (^(hexCharCount & 0xFFF) + 1) & 0xF
	field := (lchksum << 12) | (hexCharCount & 0xFFF)
	return fmt.Sprintf("%04X", field)
}

// buildCommand assembles a wire frame for the given CID2 command against the
// pack at addr, with the single address byte as the hex-encoded INFO payload —
// a faithful port of build_command (src/serial_reader.py:32-38). It is the
// shared core of BuildAnalogRequest and the wake-up ping.
func buildCommand(addr, cid2 int) []byte {
	infoHex := fmt.Sprintf("%02X", addr&0xFF)
	lengthHex := encodeLength(len(infoHex))
	inner := fmt.Sprintf("%02X%02X%02X%02X%s%s", Ver, addr&0xFF, CID1, cid2, lengthHex, infoHex)
	frame := fmt.Sprintf("%c%s%s%c", SOI, inner, checksum(inner), EOI)
	return []byte(frame)
}

// BuildAnalogRequest builds the wire frame requesting analog data from the pack
// at addr. The INFO payload is the single address byte, hex-encoded. For
// address 0 the result is the reference's verified frame
// "~21004642E00200FD36\r" (src/serial_reader.py:43-44, src/main.py:44).
func BuildAnalogRequest(addr int) []byte {
	return buildCommand(addr, CID2Analog)
}

// frame holds a parsed PACE response: the header fields, the decoded INFO
// payload, and the result of the (non-fatal) response-checksum verification.
type frame struct {
	Ver        int
	Addr       int
	CID1       int
	CID2       int    // in a response this position carries the return code (RTN)
	Info       []byte // decoded INFO payload bytes
	Checksum   string // checksum field as received (4 hex chars)
	ChecksumOK bool   // true if the recomputed checksum matches; non-fatal otherwise (Goals §3)
}

// parseFrame parses a cleaned ASCII-hex response frame (SOI/EOI already
// stripped, hex characters only, uppercased). It splits the fixed header
// (ver/addr/cid1/cid2/length), takes info_hex_len from the low 12 bits of the
// length field, and extracts the INFO payload and trailing checksum field. It
// rejects short frames, odd INFO lengths, truncated payloads (ErrFraming) and
// non-zero RTN codes (ErrBMS).
//
// Per the Goals §3 decision the response checksum is recomputed and compared,
// but a mismatch is NON-FATAL: it logs a warning and sets ChecksumOK = false
// rather than returning an error. The reference omits this check entirely
// (src/serial_reader.py:180,191); Slice 1 surfaces it for diagnostics only.
func parseFrame(cleanHex string) (frame, error) {
	if len(cleanHex) < 16 {
		return frame{}, fmt.Errorf("%w: frame too short (%d hex chars)", ErrFraming, len(cleanHex))
	}

	ver, err := strconv.ParseUint(cleanHex[0:2], 16, 8)
	if err != nil {
		return frame{}, fmt.Errorf("%w: bad ver field: %v", ErrFraming, err)
	}
	addr, err := strconv.ParseUint(cleanHex[2:4], 16, 8)
	if err != nil {
		return frame{}, fmt.Errorf("%w: bad addr field: %v", ErrFraming, err)
	}
	cid1, err := strconv.ParseUint(cleanHex[4:6], 16, 8)
	if err != nil {
		return frame{}, fmt.Errorf("%w: bad cid1 field: %v", ErrFraming, err)
	}
	cid2, err := strconv.ParseUint(cleanHex[6:8], 16, 8)
	if err != nil {
		return frame{}, fmt.Errorf("%w: bad cid2 field: %v", ErrFraming, err)
	}
	lengthField, err := strconv.ParseUint(cleanHex[8:12], 16, 16)
	if err != nil {
		return frame{}, fmt.Errorf("%w: bad length field: %v", ErrFraming, err)
	}

	infoHexLen := int(lengthField & 0x0FFF)
	if infoHexLen%2 != 0 {
		return frame{}, fmt.Errorf("%w: odd INFO length (%d) - corrupted frame", ErrFraming, infoHexLen)
	}

	// Bounds-check before slicing so a truncated frame yields ErrFraming rather
	// than a panic (the reference relies on forgiving Python slicing here).
	if len(cleanHex) < 12+infoHexLen+4 {
		return frame{}, fmt.Errorf("%w: truncated frame: need %d hex chars for INFO+checksum, have %d",
			ErrFraming, 12+infoHexLen+4, len(cleanHex))
	}

	infoHex := cleanHex[12 : 12+infoHexLen]
	chksumHex := cleanHex[12+infoHexLen : 12+infoHexLen+4]

	info, err := hex.DecodeString(infoHex)
	if err != nil {
		return frame{}, fmt.Errorf("%w: bad INFO hex: %v", ErrFraming, err)
	}

	// In a response the cid2-position byte is the return code (src/serial_reader.py:222-223).
	if int(cid2) != RTNOk {
		return frame{}, fmt.Errorf("%w: RTN code 0x%02X", ErrBMS, cid2)
	}

	// Recompute and compare the checksum over the inner hex (ver..INFO). A
	// mismatch is non-fatal: warn and continue with ChecksumOK = false.
	want := checksum(cleanHex[:12+infoHexLen])
	checksumOK := strings.EqualFold(want, chksumHex)
	if !checksumOK {
		slog.Warn("bms: response checksum mismatch (non-fatal)", "want", want, "got", chksumHex)
	}

	return frame{
		Ver:        int(ver),
		Addr:       int(addr),
		CID1:       int(cid1),
		CID2:       int(cid2),
		Info:       info,
		Checksum:   chksumHex,
		ChecksumOK: checksumOK,
	}, nil
}
