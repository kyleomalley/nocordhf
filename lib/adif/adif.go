// Package adif writes and reads Amateur Data Interchange Format (ADIF) log files.
package adif

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Record holds all fields for a single ADIF QSO entry.
type Record struct {
	TheirCall   string
	TheirGrid   string
	Mode        string // e.g. "FT8"
	RSTSent     int    // SNR they reported for our signal
	RSTRcvd     int    // SNR we measured for their signal
	TimeOn      time.Time
	TimeOff     time.Time
	Band        string  // e.g. "20m"
	FreqMHz     float64 // e.g. 14.074000
	StationCall string  // our callsign
	MyGrid      string  // our grid
}

// FormatRecord produces a single ADIF record string terminated by <EOR>.
func FormatRecord(r Record) string {
	var b strings.Builder
	writeField(&b, "CALL", r.TheirCall)
	if r.TheirGrid != "" {
		writeField(&b, "GRIDSQUARE", r.TheirGrid)
	}
	writeField(&b, "MODE", r.Mode)
	writeField(&b, "RST_SENT", fmt.Sprintf("%+d", r.RSTSent))
	writeField(&b, "RST_RCVD", fmt.Sprintf("%+d", r.RSTRcvd))
	writeField(&b, "QSO_DATE", r.TimeOn.UTC().Format("20060102"))
	writeField(&b, "TIME_ON", r.TimeOn.UTC().Format("150405"))
	writeField(&b, "QSO_DATE_OFF", r.TimeOff.UTC().Format("20060102"))
	writeField(&b, "TIME_OFF", r.TimeOff.UTC().Format("150405"))
	writeField(&b, "BAND", r.Band)
	writeField(&b, "FREQ", fmt.Sprintf("%.6f", r.FreqMHz))
	writeField(&b, "STATION_CALLSIGN", r.StationCall)
	writeField(&b, "MY_GRIDSQUARE", r.MyGrid)
	b.WriteString("<EOR>\n")
	return b.String()
}

func writeField(b *strings.Builder, tag, value string) {
	fmt.Fprintf(b, "<%s:%d>%s ", tag, len(value), value)
}

const header = "ADIF Export from nocordhf\n<EOH>\n"

// Read parses an existing ADIF file and returns all valid QSO records.
// Returns nil, nil if the file does not exist.
func Read(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	s := string(data)

	// Skip everything before <EOH>.
	if i := strings.Index(strings.ToUpper(s), "<EOH>"); i >= 0 {
		s = s[i+5:]
	}

	var records []Record
	upper := strings.ToUpper(s)
	for {
		eor := strings.Index(upper, "<EOR>")
		if eor < 0 {
			break
		}
		chunk := s[:eor]
		chunkUpper := upper[:eor]
		s = s[eor+5:]
		upper = upper[eor+5:]

		fields := parseADIFFields(chunk, chunkUpper)
		if fields["CALL"] == "" {
			continue
		}

		r := Record{
			TheirCall:   strings.TrimSpace(fields["CALL"]),
			TheirGrid:   strings.TrimSpace(fields["GRIDSQUARE"]),
			Mode:        strings.TrimSpace(fields["MODE"]),
			Band:        strings.TrimSpace(fields["BAND"]),
			StationCall: strings.TrimSpace(fields["STATION_CALLSIGN"]),
			MyGrid:      strings.TrimSpace(fields["MY_GRIDSQUARE"]),
		}
		r.RSTSent, _ = strconv.Atoi(strings.TrimSpace(fields["RST_SENT"]))
		r.RSTRcvd, _ = strconv.Atoi(strings.TrimSpace(fields["RST_RCVD"]))
		r.FreqMHz, _ = strconv.ParseFloat(strings.TrimSpace(fields["FREQ"]), 64)
		r.TimeOn = adifDateTime(fields["QSO_DATE"], fields["TIME_ON"])
		r.TimeOff = adifDateTime(fields["QSO_DATE_OFF"], fields["TIME_OFF"])

		records = append(records, r)
	}
	return records, nil
}

// parseADIFFields extracts field values from one ADIF record chunk.
// chunk and chunkUpper must be the same string, the latter already uppercased.
func parseADIFFields(chunk, chunkUpper string) map[string]string {
	fields := make(map[string]string)
	pos := 0
	for pos < len(chunkUpper) {
		lt := strings.IndexByte(chunkUpper[pos:], '<')
		if lt < 0 {
			break
		}
		lt += pos
		gt := strings.IndexByte(chunkUpper[lt:], '>')
		if gt < 0 {
			break
		}
		gt += lt + 1 // gt is now the index just past '>'

		tag := chunkUpper[lt+1 : gt-1] // e.g. "CALL:6" or "RST_SENT:3"
		pos = gt

		colon := strings.IndexByte(tag, ':')
		if colon < 0 {
			continue // no length — EOR/EOH marker, skip
		}
		name := tag[:colon]
		lenStr := tag[colon+1:]
		// Strip optional type indicator: "FIELD:6:S" → "6"
		if c := strings.IndexByte(lenStr, ':'); c >= 0 {
			lenStr = lenStr[:c]
		}
		length, err := strconv.Atoi(lenStr)
		if err != nil || length < 0 || pos+length > len(chunk) {
			continue
		}
		fields[name] = chunk[pos : pos+length]
		pos += length
	}
	return fields
}

// adifDateTime combines a QSO_DATE ("20060102") and TIME_ON ("150405") into a UTC time.
func adifDateTime(dateStr, timeStr string) time.Time {
	d := strings.TrimSpace(dateStr)
	t := strings.TrimSpace(timeStr)
	if d == "" {
		return time.Time{}
	}
	switch len(t) {
	case 0:
		t = "000000"
	case 4:
		t += "00"
	default:
		if len(t) > 6 {
			t = t[:6]
		}
	}
	ts, err := time.ParseInLocation("20060102150405", d+t, time.UTC)
	if err != nil {
		return time.Time{}
	}
	return ts
}

// Writer appends ADIF records to a file.
type Writer struct {
	path string
	mu   sync.Mutex
}

// NewWriter creates a Writer that appends to the given file path.
func NewWriter(path string) *Writer {
	return &Writer{path: path}
}

// Path returns the ADIF file path.
func (w *Writer) Path() string { return w.path }

// Append writes a single QSO record to the ADIF file.
// The header is written automatically if the file is empty or new.
func (w *Writer) Append(r Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("adif: open %s: %w", w.path, err)
	}
	defer f.Close()

	// Write header if file is empty.
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("adif: stat %s: %w", w.path, err)
	}
	if info.Size() == 0 {
		if _, err := f.WriteString(header); err != nil {
			return fmt.Errorf("adif: write header: %w", err)
		}
	}

	if _, err := f.WriteString(FormatRecord(r)); err != nil {
		return fmt.Errorf("adif: write record: %w", err)
	}
	return nil
}
