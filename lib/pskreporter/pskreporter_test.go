package pskreporter

import (
	"strings"
	"testing"
)

const sampleXML = `<?xml version="1.0" encoding="UTF-8"?>
<receptionReports>
  <activeReceiver callsign="W1AW" frequency="14074000" mode="FT8"/>
  <activeReceiver callsign="K0XYZ" frequency="14074500" mode="FT8"/>
  <activeReceiver callsign="VE3ABC" frequency="14074000" mode="FT8"/>
  <receptionReport receiverCallsign="W1AW" senderCallsign="KO6IEH" frequency="14074200" mode="FT8" sNR="-10"/>
  <receptionReport receiverCallsign="K0XYZ" senderCallsign="KO6IEH" frequency="14074200" mode="FT8" sNR="-12"/>
  <receptionReport receiverCallsign="VE3ABC" senderCallsign="N5IF" frequency="14074600" mode="FT8" sNR="-5"/>
  <receptionReport receiverCallsign="W1AW" senderCallsign="JA1ZZZ" frequency="14075000" mode="FT8" sNR="-18"/>
  <lastSequenceNumber sequenceNumber="12345"/>
</receptionReports>`

func TestParseXMLCounts(t *testing.T) {
	reports, monitors, err := parseXML(strings.NewReader(sampleXML))
	if err != nil {
		t.Fatalf("parseXML: %v", err)
	}
	if len(monitors) != 3 {
		t.Errorf("monitors = %d, want 3", len(monitors))
	}
	if len(reports) != 4 {
		t.Errorf("reports = %d, want 4", len(reports))
	}
	// Report frequencies should be preserved so callers can bucket by band.
	wantFreqs := []uint64{14074200, 14074200, 14074600, 14075000}
	for i, got := range reports {
		if got != wantFreqs[i] {
			t.Errorf("reports[%d] = %d, want %d", i, got, wantFreqs[i])
		}
	}
}

func TestParseXMLEmpty(t *testing.T) {
	empty := `<?xml version="1.0"?><receptionReports></receptionReports>`
	reports, monitors, err := parseXML(strings.NewReader(empty))
	if err != nil {
		t.Fatalf("parseXML: %v", err)
	}
	if len(reports) != 0 || len(monitors) != 0 {
		t.Errorf("expected zero counts, got reports=%d monitors=%d", len(reports), len(monitors))
	}
}

func TestBandForFreq(t *testing.T) {
	bands := []BandSpec{
		{Name: "40m", LowerHz: 7_000_000, UpperHz: 7_300_000},
		{Name: "20m", LowerHz: 14_000_000, UpperHz: 14_350_000},
	}
	cases := []struct {
		freq uint64
		want string
		ok   bool
	}{
		{14074200, "20m", true},
		{7074000, "40m", true},
		{10136000, "", false},
	}
	for _, tc := range cases {
		got, ok := bandForFreq(tc.freq, bands)
		if got != tc.want || ok != tc.ok {
			t.Errorf("bandForFreq(%d) = (%q,%v), want (%q,%v)", tc.freq, got, ok, tc.want, tc.ok)
		}
	}
}

func TestTierFor(t *testing.T) {
	cases := []struct {
		n    int
		want Tier
	}{
		{0, TierQuiet},
		{1, TierLow},
		{20, TierLow},
		{21, TierMedium},
		{100, TierMedium},
		{101, TierHigh},
		{10000, TierHigh},
	}
	for _, tc := range cases {
		if got := TierFor(tc.n); got != tc.want {
			t.Errorf("TierFor(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

func TestTierDots(t *testing.T) {
	cases := []struct {
		t    Tier
		want string
	}{
		{TierQuiet, ""},
		{TierLow, "\u2022"},
		{TierMedium, "\u2022\u2022"},
		{TierHigh, "\u2022\u2022\u2022"},
	}
	for _, tc := range cases {
		if got := tc.t.Dots(); got != tc.want {
			t.Errorf("Tier(%d).Dots() = %q, want %q", tc.t, got, tc.want)
		}
	}
}
