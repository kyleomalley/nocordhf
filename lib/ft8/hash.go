package ft8

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// FT8 callsign hashes, matching ft8_lib's save_callsign() in message.c.
//
// The wire format hashes are derived from the base-38 packed representation
// of the callsign, not from a byte-string hash. To compute:
//
//   1. Uppercase and pad the callsign to 11 characters with trailing spaces.
//   2. Pack into n58 using the ALPHANUM_SPACE_SLASH alphabet
//      ({space,0..9,A..Z,/} = indices 0..37) as n58 = Σ (digit_i * 38^(10-i)).
//   3. Compute n22 = (0xAF_6618_9F3ull * n58) >> 42, masked to 22 bits.
//   4. n12 = n22 >> 10; n10 = n22 >> 12.
//
// Note the constant 47055833459 decimal = 0xAF_6618_9F3 hex.

const hashMultiplier = uint64(47055833459)

func packN58(call string) (uint64, bool) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" || len(call) > 11 {
		return 0, false
	}
	var n58 uint64
	for i := 0; i < 11; i++ {
		var ch byte = ' '
		if i < len(call) {
			ch = call[i]
		}
		j := ncharAlphanumSpaceSlash(ch)
		if j < 0 {
			return 0, false
		}
		n58 = n58*38 + uint64(j)
	}
	return n58, true
}

func ncharAlphanumSpaceSlash(c byte) int {
	switch {
	case c == ' ':
		return 0
	case c >= '0' && c <= '9':
		return int(c-'0') + 1
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 11
	case c == '/':
		return 37
	}
	return -1
}

func hash22(call string) uint32 {
	n58, ok := packN58(call)
	if !ok {
		return 0
	}
	return uint32((hashMultiplier*n58)>>(64-22)) & 0x3FFFFF
}

func hash12(call string) uint16 { return uint16(hash22(call) >> 10) }
func hash10(call string) uint16 { return uint16(hash22(call) >> 12) }

var (
	callHashMu  sync.RWMutex
	hash22Table = map[uint32]string{}
	hash12Table = map[uint16]string{}
	hash10Table = map[uint16]string{}
)

// RegisterCallsign adds a callsign to the hash resolution tables so that
// incoming messages that reference it by 10/12/22-bit hash can be decoded
// with the full callsign. Safe to call from any goroutine.
func RegisterCallsign(call string) {
	call = strings.ToUpper(strings.TrimSpace(call))
	if call == "" || strings.HasPrefix(call, "<") {
		return
	}
	n22 := hash22(call)
	if n22 == 0 && call != "" { // packN58 rejected it (bad chars / too long)
		return
	}
	callHashMu.Lock()
	hash22Table[n22] = call
	hash12Table[uint16(n22>>10)] = call
	hash10Table[uint16(n22>>12)] = call
	callHashMu.Unlock()
}

// lookupHash12 resolves a 12-bit hash to a callsign.
// Returns the callsign wrapped in angle brackets (matching reference design display)
// if registered, otherwise a "<N>" numeric placeholder.
func lookupHash12(h uint16) string {
	callHashMu.RLock()
	call, ok := hash12Table[h]
	callHashMu.RUnlock()
	if ok {
		return "<" + call + ">"
	}
	return fmt.Sprintf("<%d>", h)
}

// lookupHash22 resolves a 22-bit hash to a callsign, or returns a
// "<...N>" placeholder matching reference design display convention.
func lookupHash22(h uint32) string {
	callHashMu.RLock()
	call, ok := hash22Table[h]
	callHashMu.RUnlock()
	if ok {
		return "<" + call + ">"
	}
	return fmt.Sprintf("<...%d>", h)
}

// knownCallsigns returns every callsign we have registered, for persistence.
// Returned list is sorted for stable file output.
func knownCallsigns() []string {
	callHashMu.RLock()
	seen := make(map[string]struct{}, len(hash22Table))
	for _, c := range hash22Table {
		seen[c] = struct{}{}
	}
	callHashMu.RUnlock()
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// LoadCallsignCache reads one callsign per line from path and registers each.
// Missing file is not an error. Malformed lines are skipped.
func LoadCallsignCache(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<16)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		RegisterCallsign(line)
	}
	return sc.Err()
}

// SaveCallsignCache writes every known callsign to path, one per line. Creates
// parent directories as needed. Atomic via tmp-file + rename.
func SaveCallsignCache(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".callsigns-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	bw := bufio.NewWriter(tmp)
	for _, c := range knownCallsigns() {
		if _, err := bw.WriteString(c); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return err
		}
		if err := bw.WriteByte('\n'); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
