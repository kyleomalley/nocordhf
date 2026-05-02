package nocord

// auto_reply.go — stateless reactive QSO progression.
//
// autoReplyTail looks at one inbound decoded message and, if it's
// directed at us with a recognized trailing token, returns the trailing
// token of the appropriate next outbound message. The caller assembles
// "<them> <us> <tail>" into a TxRequest. No state is kept between
// calls — every reply is a pure function of the message just heard.
//
// Mapping:
//
//	"<us> <them> <grid>"  → "<their_snr_at_us>"   (CQ-reply scenario)
//	"<us> <them> ±NN"     → "R<their_snr_at_us>"  (their report → R+ours)
//	"<us> <them> R±NN"    → "RR73"                (their R+report → RR73)
//	"<us> <them> RR73"    → "73"                  (close)
//	"<us> <them> 73"      → ""                    (they closed; nothing)

import (
	"fmt"
	"strings"
	"time"
)

// Retry policy for auto-replies: how many times we'll re-send the same
// trailer to a station that hasn't responded, and how long we wait
// between attempts. ~30 s = one of their TX slots + one of ours, the
// minimum window for them to have heard our last TX and replied.
const (
	retryMaxAttempts = 4
	retryWait        = 30 * time.Second
)

// pendingRetry tracks one auto-reply in flight: the trailer we sent,
// how many times we've TX'd it, and when we last did. Re-queued by the
// 1 Hz status ticker until either we see a response from `remote` (the
// entry is then cleared) or attempts hits retryMaxAttempts (dropped
// with a log line so a missed weak signal is at least visible).
type pendingRetry struct {
	tail     string
	attempts int
	lastSent time.Time
}

// autoReplyTail returns the trailer to put after "<them> <us>" in the
// auto-progress reply. ourSNRofThem is the SNR estimate from the
// decoded signal (typically rounded from ft8.Decoded.SNR).
//
// Returns "" when no auto-reply applies: rxMsg isn't directed at us,
// the trailing token isn't a recognized QSO step, or the QSO has just
// closed (their 73 / our prior 73).
func autoReplyTail(rxMsg, ourCall string, ourSNRofThem int) string {
	fields := strings.Fields(strings.ToUpper(strings.TrimSpace(rxMsg)))
	if len(fields) < 3 {
		return ""
	}
	if fields[0] != strings.ToUpper(strings.TrimSpace(ourCall)) {
		return ""
	}
	last := fields[len(fields)-1]
	switch {
	case last == "73":
		// They closed the QSO; we don't TX anything.
		return ""
	case last == "RR73":
		return "73"
	case len(last) >= 3 && last[0] == 'R' && (last[1] == '+' || last[1] == '-'):
		// They sent us R+report; acknowledge with RR73.
		return "RR73"
	case len(last) >= 2 && (last[0] == '+' || last[0] == '-'):
		// They sent us a bare sig report; reply with R + our report of them.
		return fmt.Sprintf("R%+d", ourSNRofThem)
	case isGridLike(last):
		// They answered our directed call with their grid; we owe them a
		// sig report (no R prefix on the first one).
		return fmt.Sprintf("%+d", ourSNRofThem)
	}
	return ""
}
