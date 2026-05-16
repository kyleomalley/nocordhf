package nocord

// diagnostics.go — operator-triggered "save a diagnostic bundle"
// that packages the small handful of files an operator + maintainer
// usually need to triage a bug report into one zip.
//
// Default contents:
//   - summary.txt       runtime info (OS, build, memory, etc.)
//   - nocordhf.log      last ~2 MB of the rolling log
//   - prefs.json        operator prefs with credentials redacted
//
// Sensitive-by-default items are EXCLUDED unless the operator
// explicitly opts in via the dialog: the bbolt chat-history store,
// recent TX WAV files, and the full unredacted prefs file. The
// included redacted prefs.json strips known credential keys
// (LoTW / TQSL passwords + usernames) before writing.
//
// The zip writes via the operator-chosen path so a GitHub-issue
// upload is "Save → drag the .zip into the comment".

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

// DiagnosticOptions controls which sensitive-by-default items the
// bundle includes. Default zero value omits everything that could
// leak operator state.
type DiagnosticOptions struct {
	// IncludeChatHistory copies nocordhf-meshcore.db into the bundle.
	// Contains the operator's persisted contacts, channels, pending
	// adverts, and per-thread chat-history buckets. Useful for
	// reproducing channel-keying / message-render bugs; do NOT
	// include for a generic bug report.
	IncludeChatHistory bool
	// IncludeRecordings copies the last N tx_debug_*.wav files from
	// recordings/. Useful for "did my TX sound wrong?" diagnostics;
	// each wav is ~1.4 MB.
	IncludeRecordings bool
	// IncludeUnredactedPrefs copies the raw preferences.json file
	// verbatim — credentials and all. Only useful if reproducing
	// a preferences-serialisation bug and the operator trusts the
	// recipient.
	IncludeUnredactedPrefs bool
	// MaxLogBytes caps how much of nocordhf.log gets included
	// (from the tail). 0 picks a reasonable default.
	MaxLogBytes int64
}

// keys we know carry credentials or other secret-shaped state.
// Redaction is best-effort: any future credential pref needs to be
// added here to avoid leaking into a posted bundle.
var diagSensitivePrefKeys = map[string]bool{
	"lotw_password":      true,
	"tqsl_cert_password": true,
	// Username is less sensitive but identifies the operator on a
	// public-internet service; included in the redaction set so
	// "I posted my diag in the issue" doesn't dox them.
	"lotw_username":   true,
	"tqsl_station":    true,
	"radio_port":      true, // device path can reveal username / mount points on some setups
	"audio_rx_device": true,
	"audio_tx_device": true,
}

// saveDiagnosticBundle writes a zip containing the chosen
// diagnostic artifacts to dst. Returns an error only when the
// final write fails; individual file-include failures are logged
// into summary.txt so a partial bundle still ships rather than
// the whole save aborting.
func saveDiagnosticBundle(dst string, opts DiagnosticOptions) error {
	if opts.MaxLogBytes <= 0 {
		opts.MaxLogBytes = 2 * 1024 * 1024 // 2 MB tail
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()

	// Summary first so it's the first entry the recipient sees.
	notes := &strings.Builder{}
	fmt.Fprintf(notes, "NocordHF diagnostic bundle\n")
	fmt.Fprintf(notes, "Generated: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(notes, "Bundle options: chat_history=%t recordings=%t unredacted_prefs=%t\n\n",
		opts.IncludeChatHistory, opts.IncludeRecordings, opts.IncludeUnredactedPrefs)

	fmt.Fprintf(notes, "## Runtime\n")
	fmt.Fprintf(notes, "go_version : %s\n", runtime.Version())
	fmt.Fprintf(notes, "goos       : %s\n", runtime.GOOS)
	fmt.Fprintf(notes, "goarch     : %s\n", runtime.GOARCH)
	fmt.Fprintf(notes, "num_cpu    : %d\n", runtime.NumCPU())
	fmt.Fprintf(notes, "goroutines : %d\n", runtime.NumGoroutine())

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Fprintf(notes, "heap_alloc : %s\n", humanBytes(int64(ms.HeapAlloc)))
	fmt.Fprintf(notes, "sys_total  : %s\n", humanBytes(int64(ms.Sys)))
	fmt.Fprintf(notes, "num_gc     : %d\n\n", ms.NumGC)

	if info, ok := debug.ReadBuildInfo(); ok {
		fmt.Fprintf(notes, "## Build\n")
		fmt.Fprintf(notes, "main_module : %s %s\n", info.Main.Path, info.Main.Version)
		for _, s := range info.Settings {
			// Pull out vcs.* settings which carry the git commit
			// when the binary was built with module-mode go build.
			if strings.HasPrefix(s.Key, "vcs.") {
				fmt.Fprintf(notes, "%-12s: %s\n", s.Key, s.Value)
			}
		}
		fmt.Fprint(notes, "\n")
	}

	// File-include attempts. Each helper appends to notes on failure
	// so the bundle ships partial rather than aborting on the first
	// missing file.
	addFile := func(name, srcPath string, maxTailBytes int64) {
		if srcPath == "" {
			return
		}
		st, err := os.Stat(srcPath)
		if err != nil {
			fmt.Fprintf(notes, "skipped %s — %s\n", name, err)
			return
		}
		src, err := os.Open(srcPath)
		if err != nil {
			fmt.Fprintf(notes, "skipped %s — %s\n", name, err)
			return
		}
		defer src.Close()
		// Seek to the tail when the file exceeds the cap (useful for
		// log files where the relevant context is near the end).
		if maxTailBytes > 0 && st.Size() > maxTailBytes {
			if _, err := src.Seek(st.Size()-maxTailBytes, io.SeekStart); err != nil {
				fmt.Fprintf(notes, "skipped %s — seek: %s\n", name, err)
				return
			}
		}
		w, err := zw.Create(name)
		if err != nil {
			fmt.Fprintf(notes, "skipped %s — zip header: %s\n", name, err)
			return
		}
		if _, err := io.Copy(w, src); err != nil {
			fmt.Fprintf(notes, "skipped %s — copy: %s\n", name, err)
			return
		}
		fmt.Fprintf(notes, "included %s (%s%s)\n",
			name, humanBytes(min64(st.Size(), maxTailBytes)),
			func() string {
				if maxTailBytes > 0 && st.Size() > maxTailBytes {
					return " [tail of " + humanBytes(st.Size()) + "]"
				}
				return ""
			}())
	}

	fmt.Fprintf(notes, "\n## Files\n")
	addFile("nocordhf.log", "nocordhf.log", opts.MaxLogBytes)
	addFile("nocordhf-meshcore.log", "nocordhf-meshcore.log", opts.MaxLogBytes)
	addFile("nocordhf-stderr.log", "nocordhf-stderr.log", 0)

	// Preferences — redacted or raw depending on the operator's pick.
	prefsPath := guessFynePrefsPath()
	if opts.IncludeUnredactedPrefs {
		addFile("prefs-raw.json", prefsPath, 0)
	} else if prefsPath != "" {
		if redacted, err := redactedPrefsJSON(prefsPath); err != nil {
			fmt.Fprintf(notes, "skipped prefs.json — %s\n", err)
		} else {
			w, err := zw.Create("prefs.json")
			if err == nil {
				_, _ = w.Write(redacted)
				fmt.Fprintf(notes, "included prefs.json (%s, credentials redacted)\n",
					humanBytes(int64(len(redacted))))
			}
		}
	}

	if opts.IncludeChatHistory {
		addFile("nocordhf-meshcore.db", "nocordhf-meshcore.db", 0)
	}

	if opts.IncludeRecordings {
		// Last 3 TX debug WAVs from recordings/. Newest first.
		const wantWAVs = 3
		entries, err := os.ReadDir("recordings")
		if err == nil {
			type rec struct {
				name string
				mtim time.Time
			}
			var all []rec
			for _, e := range entries {
				if e.IsDir() || !strings.HasPrefix(e.Name(), "tx_debug_") || !strings.HasSuffix(e.Name(), ".wav") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				all = append(all, rec{e.Name(), info.ModTime()})
			}
			sort.Slice(all, func(i, j int) bool { return all[i].mtim.After(all[j].mtim) })
			for i, r := range all {
				if i >= wantWAVs {
					break
				}
				addFile("recordings/"+r.name, filepath.Join("recordings", r.name), 0)
			}
		}
	}

	w, err := zw.Create("summary.txt")
	if err != nil {
		return fmt.Errorf("zip summary header: %w", err)
	}
	if _, err := w.Write([]byte(notes.String())); err != nil {
		return fmt.Errorf("zip summary body: %w", err)
	}
	return nil
}

// guessFynePrefsPath returns the platform-specific path to the
// Fyne preferences.json. Returns "" when the path can't be
// constructed (e.g. UserConfigDir failure on weird sandbox
// environments).
func guessFynePrefsPath() string {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(cfg, "fyne", "com.nocordhf.app", "preferences.json")
}

// redactedPrefsJSON reads the raw prefs file and returns a JSON
// blob with known-sensitive keys replaced by the literal
// "[REDACTED]". Preserves the rest of the keys verbatim so the
// recipient still sees pref keys + non-sensitive values.
func redactedPrefsJSON(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse prefs: %w", err)
	}
	for k := range doc {
		if diagSensitivePrefKeys[strings.ToLower(k)] {
			doc[k] = "[REDACTED]"
		}
	}
	// Pretty-print so a human reviewing the bundle can skim it.
	return json.MarshalIndent(doc, "", "  ")
}

// humanBytes formats a byte count in IEC-ish units (1024-based)
// for display in the summary text.
func humanBytes(n int64) string {
	if n < 0 {
		return "0 B"
	}
	const (
		kib = 1024
		mib = kib * 1024
		gib = mib * 1024
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func min64(a, b int64) int64 {
	if b > 0 && b < a {
		return b
	}
	return a
}
