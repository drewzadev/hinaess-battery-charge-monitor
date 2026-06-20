// This file defines the decoded data model — PackSample and Temp — together
// with the sentinel errors callers and tests use to distinguish failure modes.
// Field set and units follow requirements.md:162-171; the SOC source (raw u8
// soc_int) and lowercased probe labels follow the PRD's Q2/Q3 decisions.
package bms

import (
	"errors"
	"time"
)

// PackSample is one decoded analog reading from the BMS.
type PackSample struct {
	Timestamp time.Time `json:"ts"`         // host clock at receive time
	Cells     []int     `json:"cells_mv"`   // N cell voltages, mV (expect 16)
	Temps     []Temp    `json:"temps"`      // up to 6, labeled
	PackMV    int       `json:"pack_mv"`    // pack voltage, mV
	PackMA    int       `json:"pack_ma"`    // signed: + charge, − discharge
	SOCPct    float64   `json:"soc_pct"`    // BMS raw u8 soc_int, per Q2 (Option B)
	SOHPct    float64   `json:"soh_pct"`    // state of health, %
	Cycles    int       `json:"cycles"`     // charge/discharge cycle count
	RemainMAH int       `json:"remain_mah"` // remaining capacity, mAh
	FullMAH   int       `json:"full_mah"`   // full capacity, mAh
}

// Temp is one temperature probe reading.
type Temp struct {
	Probe string `json:"probe"`  // "t1".."t4","mos","env" by index (lowercased per Q3, Option A)
	DeciC int    `json:"deci_c"` // tenths of a degree C (raw − 2731)
}

// Sentinel errors so callers and tests can distinguish failure modes
// (requirements.md:173). A response-checksum mismatch is deliberately NOT a
// sentinel error in this slice — per the Goals §3 decision it is non-fatal and
// surfaced via a ChecksumOK bool on the decoded frame, not returned as an error.
var (
	// ErrTimeout is returned when no complete frame arrives before the read deadline.
	ErrTimeout = errors.New("bms: read timeout")
	// ErrFraming is returned for malformed frames: too short, odd INFO length,
	// or a truncated payload.
	ErrFraming = errors.New("bms: framing error")
	// ErrImplausible is returned when decoded core values fall outside sane
	// ranges, so the caller discards the sample rather than reporting garbage.
	ErrImplausible = errors.New("bms: implausible decoded values")
	// ErrBMS is returned when the response carries a non-zero RTN code.
	ErrBMS = errors.New("bms: device returned error code")
)
