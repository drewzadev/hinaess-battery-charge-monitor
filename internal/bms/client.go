// This file implements the serial client: opening the RS485 port 8N1, running
// one analog exchange (wake ping → analog request → read-until-EOI → parseFrame
// → ParseAnalog), and closing the port. It ports the serial behaviour of the
// reference (src/serial_reader.py:61-113, src/main.py:91-192): wake the bus
// before polling, read byte-wise until the EOI marker, strip everything before
// the SOI, keep only hex characters, and retry the analog read up to twice.
package bms

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"go.bug.st/serial"
)

const (
	// analogAttempts is the number of analog reads tried per Poll before giving
	// up — one initial read plus one retry, mirroring the reference's two-attempt
	// loop with a bus flush between attempts (src/main.py:170-192).
	analogAttempts = 2

	// readPollInterval bounds how long a single port Read blocks before returning
	// with no data, so the overall read deadline in readUntilEOI is checked
	// regularly (the reference polls in_waiting on a 10 ms tick, src/serial_reader.py:96).
	readPollInterval = 100 * time.Millisecond

	// wakeSettle is the pause after the wake-up ping before the analog request,
	// letting a cold BMS come up (the reference sleeps 0.1 s + 0.5 s, src/main.py:96-98).
	wakeSettle = 500 * time.Millisecond
	// wakeReadTimeout caps how long we wait for (and discard) the wake reply.
	wakeReadTimeout = 500 * time.Millisecond
	// busFlushDelay is the settle time before draining leftover bytes ahead of a
	// retry (flush_bus default, src/serial_reader.py:148).
	busFlushDelay = 300 * time.Millisecond
)

// Client owns an open serial port to a single BMS pack at a fixed address.
type Client struct {
	port        serial.Port
	portName    string
	address     int
	readTimeout time.Duration
}

// Open configures and opens the serial port 8N1 at the given baud rate and
// returns a Client bound to the pack at address. readTimeout is the overall
// deadline for reading one response frame in Poll.
func Open(portName string, baud, address int, readTimeout time.Duration) (*Client, error) {
	mode := &serial.Mode{
		BaudRate: baud,
		DataBits: 8,
		Parity:   serial.NoParity,
		StopBits: serial.OneStopBit,
	}
	port, err := serial.Open(portName, mode)
	if err != nil {
		return nil, fmt.Errorf("bms: open %s: %w", portName, err)
	}
	// Use a short per-Read timeout so readUntilEOI can poll its overall deadline;
	// the logical read timeout is enforced by readUntilEOI, not the port.
	if err := port.SetReadTimeout(readPollInterval); err != nil {
		port.Close()
		return nil, fmt.Errorf("bms: set read timeout on %s: %w", portName, err)
	}
	return &Client{
		port:        port,
		portName:    portName,
		address:     address,
		readTimeout: readTimeout,
	}, nil
}

// Poll performs one analog read: it wakes the bus, writes the analog request,
// reads until the EOI marker, parses the frame and decodes the analog payload,
// and stamps the sample with the host clock at receive time. On a timeout or a
// framing/decode failure it retries the analog read up to analogAttempts times,
// flushing the bus between attempts. It always returns the raw TX bytes, and the
// RX bytes from the last attempt, so --debug can hex-dump them.
func (c *Client) Poll() (sample PackSample, tx, rx []byte, err error) {
	tx = BuildAnalogRequest(c.address)

	// Wake the bus first: a cold/asleep BMS may not answer the first analog read,
	// so send a protocol-version ping and discard whatever comes back, then let
	// the bus settle (src/main.py:91-98).
	c.wake()

	for attempt := 0; attempt < analogAttempts; attempt++ {
		if attempt > 0 {
			c.flushBus(busFlushDelay)
		}

		var clean string
		rx, clean, err = c.exchange(tx)
		if err != nil {
			slog.Debug("bms: analog read failed", "attempt", attempt+1, "err", err)
			continue
		}

		var f frame
		if f, err = parseFrame(clean); err != nil {
			slog.Debug("bms: frame parse failed", "attempt", attempt+1, "err", err)
			continue
		}

		if sample, err = ParseAnalog(f.Info); err != nil {
			slog.Debug("bms: analog decode failed", "attempt", attempt+1, "err", err)
			continue
		}

		sample.Timestamp = time.Now().UTC()
		return sample, tx, rx, nil
	}

	return PackSample{}, tx, rx, err
}

// Close closes the underlying serial port.
func (c *Client) Close() error {
	if c == nil || c.port == nil {
		return nil
	}
	return c.port.Close()
}

// wake sends a single protocol-version ping to nudge a sleeping bus awake and
// discards the reply. Failures are non-fatal — the analog read (with retries)
// is the real test of the link.
func (c *Client) wake() {
	wake := buildCommand(c.address, CID2Proto)
	if err := c.port.ResetInputBuffer(); err != nil {
		slog.Debug("bms: reset input before wake failed", "err", err)
	}
	if _, err := c.port.Write(wake); err != nil {
		slog.Debug("bms: wake write failed (non-fatal)", "err", err)
		return
	}
	_ = c.port.Drain()
	if _, err := c.readUntilEOI(wakeReadTimeout); err != nil {
		slog.Debug("bms: no wake reply (non-fatal)", "err", err)
	}
	time.Sleep(wakeSettle)
}

// exchange writes tx, then reads one response frame and returns the raw RX bytes
// alongside the cleaned ASCII-hex payload (SOI-stripped, hex-only, uppercased).
func (c *Client) exchange(tx []byte) (rx []byte, clean string, err error) {
	if err = c.port.ResetInputBuffer(); err != nil {
		return nil, "", fmt.Errorf("bms: reset input buffer: %w", err)
	}
	if _, err = c.port.Write(tx); err != nil {
		return nil, "", fmt.Errorf("bms: write request: %w", err)
	}
	if err = c.port.Drain(); err != nil {
		return nil, "", fmt.Errorf("bms: drain write buffer: %w", err)
	}
	rx, err = c.readUntilEOI(c.readTimeout)
	if err != nil {
		return rx, "", err
	}
	return rx, cleanHexPayload(rx), nil
}

// readUntilEOI reads from the port until the EOI byte is seen or the overall
// timeout elapses, returning the raw bytes received up to (not including) EOI.
// It returns ErrTimeout if no EOI arrives before the deadline. The port's own
// per-Read timeout (readPollInterval) makes idle Reads return promptly with no
// data so the deadline is honoured (src/serial_reader.py:89-94).
func (c *Client) readUntilEOI(timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 256)
	var rx []byte

	for time.Now().Before(deadline) {
		n, err := c.port.Read(buf)
		if err != nil {
			return rx, fmt.Errorf("bms: serial read: %w", err)
		}
		if n == 0 {
			continue // per-Read timeout with no data; keep waiting until deadline
		}
		if i := bytes.IndexByte(buf[:n], EOI); i >= 0 {
			rx = append(rx, buf[:i]...)
			return rx, nil
		}
		rx = append(rx, buf[:n]...)
	}

	return rx, ErrTimeout
}

// flushBus settles for delay then drains any leftover bytes from the port,
// clearing partial/garbage frames before a retry (flush_bus, src/serial_reader.py:148-158).
func (c *Client) flushBus(delay time.Duration) {
	time.Sleep(delay)
	if err := c.port.ResetInputBuffer(); err != nil {
		slog.Debug("bms: bus flush failed", "err", err)
	}
}

// cleanHexPayload extracts the clean ASCII-hex response from raw RX bytes: drop
// everything up to and including the SOI marker, keep only hex characters, and
// uppercase them (src/serial_reader.py:99-107).
func cleanHexPayload(rx []byte) string {
	payload := rx
	if i := bytes.IndexByte(rx, SOI); i >= 0 {
		payload = rx[i+1:]
	}
	var b strings.Builder
	b.Grow(len(payload))
	for _, ch := range payload {
		switch {
		case ch >= '0' && ch <= '9', ch >= 'A' && ch <= 'F':
			b.WriteByte(ch)
		case ch >= 'a' && ch <= 'f':
			b.WriteByte(ch - ('a' - 'A'))
		}
	}
	return b.String()
}
