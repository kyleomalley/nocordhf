// Package ntpcheck queries public NTP servers over UDP and reports the local
// clock offset so the UI can warn when the system clock drifts too far for
// reliable FT8 operation (FT8 tolerates roughly ±0.5 s).
package ntpcheck

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

// ntpEpochOffset is the number of seconds between the NTP epoch (1 Jan 1900)
// and the Unix epoch (1 Jan 1970).
const ntpEpochOffset = 2208988800

// servers to try in order; first success wins.
var defaultServers = []string{
	"0.pool.ntp.org:123",
	"1.pool.ntp.org:123",
	"time.cloudflare.com:123",
	"time.google.com:123",
}

// Checker polls NTP servers periodically and exposes the current clock offset.
type Checker struct {
	mu     sync.Mutex
	offset time.Duration
	valid  bool
}

// New returns a Checker. Call Start to begin background polling.
func New() *Checker { return &Checker{} }

// Start launches a background goroutine that polls immediately and then every
// 5 minutes. It is a no-op if the host has no network.
func (c *Checker) Start() {
	go func() {
		c.poll()
		for range time.Tick(5 * time.Minute) {
			c.poll()
		}
	}()
}

// Offset returns the most recent measured clock offset and whether a valid
// measurement has been obtained.  Positive = local clock is behind NTP,
// negative = local clock is ahead.
func (c *Checker) Offset() (offset time.Duration, valid bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset, c.valid
}

// poll tries each server until one responds.
func (c *Checker) poll() {
	for _, srv := range defaultServers {
		off, err := query(srv)
		if err != nil {
			continue
		}
		c.mu.Lock()
		c.offset = off
		c.valid = true
		c.mu.Unlock()
		return
	}
}

// query sends one NTP client request to server and returns the clock offset.
// offset = ((T2-T1) + (T3-T4)) / 2
//
//	T1 = local transmit time
//	T2 = server receive timestamp (bytes 32-39 of response)
//	T3 = server transmit timestamp (bytes 40-47 of response)
//	T4 = local receive time
func query(server string) (time.Duration, error) {
	conn, err := net.DialTimeout("udp", server, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint

	// 48-byte NTP request: LI=0, VN=4, Mode=3 (client) → byte0 = 0b00_100_011 = 0x23
	req := make([]byte, 48)
	req[0] = 0x23

	t1 := time.Now()
	if _, err := conn.Write(req); err != nil {
		return 0, err
	}
	resp := make([]byte, 48)
	if _, err := conn.Read(resp); err != nil {
		return 0, err
	}
	t4 := time.Now()

	t2 := ntpToTime(binary.BigEndian.Uint64(resp[32:40]))
	t3 := ntpToTime(binary.BigEndian.Uint64(resp[40:48]))

	return (t2.Sub(t1) + t3.Sub(t4)) / 2, nil
}

// ntpToTime converts a 64-bit NTP fixed-point timestamp to time.Time.
func ntpToTime(ts uint64) time.Time {
	sec := int64(ts>>32) - ntpEpochOffset
	nsec := int64(uint32(ts)) * 1e9 >> 32
	return time.Unix(sec, nsec)
}
