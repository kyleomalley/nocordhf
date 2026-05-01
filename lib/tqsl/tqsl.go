// Package tqsl wraps the ARRL Trusted QSL binary for uploading
// ADIF logs to Logbook of the World (LoTW).
package tqsl

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultMacPath is the typical macOS location for the tqsl binary.
const DefaultMacPath = "/Applications/TrustedQSL/tqsl.app/Contents/MacOS/tqsl"

// Config holds the TQSL binary path, credentials, and signing options.
type Config struct {
	BinaryPath      string // path to the tqsl executable
	StationLocation string // -l flag: station location name configured in TQSL
	CertPassword    string // -p flag: certificate private key password
	Username        string // LoTW username (callsign)
	Password        string // LoTW website password (for future use / validation)
}

// Available reports whether the TQSL binary exists at BinaryPath.
func (c *Config) Available() bool {
	if c.BinaryPath == "" {
		return false
	}
	_, err := os.Stat(c.BinaryPath)
	return err == nil
}

// DataDir returns the platform-specific TQSL data directory.
func DataDir() string {
	home, _ := os.UserHomeDir()
	// On macOS, TQSL stores data in ~/.tqsl (not ~/Library/Application Support).
	// Check ~/.tqsl first (used by modern TQSL on all platforms).
	dotTQSL := filepath.Join(home, ".tqsl")
	if _, err := os.Stat(dotTQSL); err == nil {
		return dotTQSL
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "TrustedQSL")
	case "windows":
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "TrustedQSL")
	default:
		return filepath.Join(home, ".TrustedQSL")
	}
}

// stationDataFile is the XML structure used by TQSL for station locations.
type stationDataFile struct {
	XMLName  xml.Name      `xml:"StationDataFile"`
	Stations []stationData `xml:"StationData"`
}

type stationData struct {
	Name string `xml:"name,attr"`
}

// StationLocations reads the TQSL station_data file and returns
// the names of all configured station locations.
func StationLocations() ([]string, error) {
	path := filepath.Join(DataDir(), "station_data")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tqsl: read station data: %w", err)
	}
	var sdf stationDataFile
	if err := xml.Unmarshal(data, &sdf); err != nil {
		return nil, fmt.Errorf("tqsl: parse station data: %w", err)
	}
	names := make([]string, len(sdf.Stations))
	for i, s := range sdf.Stations {
		names[i] = s.Name
	}
	return names, nil
}

// Test verifies the TQSL binary can run by invoking it with the
// -v flag. Returns the version string on success.
// Note: tqsl -v exits with code 255 even on success, so we ignore
// the exit code and just check if we got version output.
func (c *Config) Test() (string, error) {
	if !c.Available() {
		return "", fmt.Errorf("tqsl binary not found at %s", c.BinaryPath)
	}
	cmd := exec.Command(c.BinaryPath, "-v")
	output, _ := cmd.CombinedOutput()
	out := strings.TrimSpace(string(output))
	if out == "" {
		return "", fmt.Errorf("tqsl: no version output from %s", c.BinaryPath)
	}
	return out, nil
}

// Upload signs and uploads the ADIF file at adifPath to LoTW.
// It runs tqsl in batch mode (-x) with upload (-u) and duplicate
// handling set to "compliant" (-a compliant).
//
// Exit codes:
//
//	0 = all QSOs uploaded successfully
//	8 = all QSOs were duplicates (treated as success)
//	9 = partial success (some records skipped)
//	other = error
func (c *Config) Upload(adifPath string) error {
	if !c.Available() {
		return fmt.Errorf("tqsl binary not found at %s", c.BinaryPath)
	}

	// Flag map for TQSL 2.7+ (`tqsl -h` output):
	//   -x  batch mode (exit after processing the log)
	//   -u  upload after signing
	//   -d  --nodate, suppress the date-range dialog
	//   -a  action when QSOs aren't signed: compliant
	//   -l  station location name (added below if set)
	//   -p  signing key passphrase (added below if set)

	args := []string{"-x", "-u", "-d", "-a", "compliant"}
	if c.StationLocation != "" {
		args = append(args, "-l", c.StationLocation)
	}
	if c.CertPassword != "" {
		args = append(args, "-p", c.CertPassword)
	}
	args = append(args, adifPath)

	cmd := exec.Command(c.BinaryPath, args...)
	output, err := cmd.CombinedOutput()
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	switch exitCode {
	case 0, 8:
		return nil // success or all duplicates
	case 9:
		return fmt.Errorf("tqsl: partial upload (some records skipped): %s", output)
	default:
		if err != nil {
			return fmt.Errorf("tqsl: exit %d: %w: %s", exitCode, err, output)
		}
		return fmt.Errorf("tqsl: exit %d: %s", exitCode, output)
	}
}
