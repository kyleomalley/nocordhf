// jt9_corpus runs jt9 (from a WSJT-X install) on a batch of WAV files and
// writes per-slot results into a versioned corpus directory, mirroring the
// layout produced by `nocordhf -decode`. The two corpora can then be diffed
// by tools/compare_corpus to track decode-quality changes.
//
// Usage:
//
//	jt9_corpus [-jt9 PATH] [-corpus-dir DIR] file1.wav [file2.wav ...]
//
// Default jt9 path: /Applications/wsjtx.app/Contents/MacOS/jt9
// Default output:   recordings/corpus/jt9/<bundle-version>/
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func main() {
	jt9Path := flag.String("jt9", "/Applications/wsjtx.app/Contents/MacOS/jt9", "path to jt9 binary")
	corpusDir := flag.String("corpus-dir", "", "output directory (default: recordings/corpus/jt9/<version>)")
	flag.Parse()

	wavs := flag.Args()
	if len(wavs) == 0 {
		fmt.Fprintf(os.Stderr, "usage: jt9_corpus [-jt9 PATH] [-corpus-dir DIR] file1.wav [...]\n")
		os.Exit(1)
	}

	if _, err := os.Stat(*jt9Path); err != nil {
		fmt.Fprintf(os.Stderr, "jt9 not found at %s: %v\n", *jt9Path, err)
		os.Exit(1)
	}

	version := jt9Version(*jt9Path)
	if *corpusDir == "" {
		*corpusDir = filepath.Join("recordings", "corpus", "jt9", version)
	}
	if err := os.MkdirAll(*corpusDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create corpus dir: %v\n", err)
		os.Exit(1)
	}

	runTime := time.Now().UTC().Format(time.RFC3339)
	fmt.Printf("Corpus dir: %s (jt9=%s)\n", *corpusDir, version)
	fmt.Printf("Running jt9 on %d file(s)...\n", len(wavs))

	// jt9 prints decodes to stdout, terminating each input file with
	// "<DecodeFinished>". By tracking that marker we can attribute the
	// preceding decode lines back to the correct input WAV.
	args := append([]string{"-8"}, wavs...)
	cmd := exec.Command(*jt9Path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stdout pipe: %v\n", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start jt9: %v\n", err)
		os.Exit(1)
	}

	type entry struct {
		freq float64
		snr  int
		msg  string
	}
	perSlot := make([][]entry, len(wavs))
	slotIdx := 0

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "<DecodeFinished>") {
			slotIdx++
			continue
		}
		if slotIdx >= len(wavs) {
			continue
		}
		// jt9 line format (example):
		//   "000000 -14  0.3 1510 ~  JO7VVK K0FBI R-11"
		//   <time> <snr> <dt> <freq> ~ <message>
		idx := strings.Index(line, " ~ ")
		if idx < 0 {
			continue
		}
		msg := strings.TrimSpace(line[idx+3:])
		if msg == "" {
			continue
		}
		fields := strings.Fields(line[:idx])
		if len(fields) < 4 {
			continue
		}
		var snr int
		fmt.Sscanf(fields[1], "%d", &snr)
		var freq float64
		fmt.Sscanf(fields[3], "%f", &freq)
		perSlot[slotIdx] = append(perSlot[slotIdx], entry{freq: freq, snr: snr, msg: msg})
	}
	if err := cmd.Wait(); err != nil {
		fmt.Fprintf(os.Stderr, "jt9 exited: %v\n", err)
	}

	// Write per-slot files: dedupe messages keeping highest SNR (matches
	// the dedup policy in cmd/nocordhf -decode so the two corpora are
	// directly diffable).
	for i, wav := range wavs {
		base := strings.TrimSuffix(filepath.Base(wav), filepath.Ext(wav))
		outPath := filepath.Join(*corpusDir, base+".txt")

		bestByMsg := make(map[string]entry)
		for _, e := range perSlot[i] {
			if existing, ok := bestByMsg[e.msg]; !ok || e.snr > existing.snr {
				bestByMsg[e.msg] = e
			}
		}
		msgs := make([]string, 0, len(bestByMsg))
		for m := range bestByMsg {
			msgs = append(msgs, m)
		}
		sort.Strings(msgs)

		f, err := os.Create(outPath)
		if err != nil {
			fmt.Printf("  %s: create failed: %v\n", base, err)
			continue
		}
		fmt.Fprintf(f, "# jt9 %s | run=%s\n", version, runTime)
		fmt.Fprintf(f, "# freq\tsnr\tmessage\n")
		for _, m := range msgs {
			e := bestByMsg[m]
			fmt.Fprintf(f, "%.1f\t%d\t%s\n", e.freq, e.snr, m)
		}
		f.Close()

		fmt.Printf("  %s: %d messages\n", base, len(msgs))
	}
}

// jt9Version reads the WSJT-X bundle version (macOS) when jt9 lives inside
// a .app, falling back to "unknown" so callers always get a non-empty
// directory name. jt9 itself has no --version flag.
func jt9Version(jt9Path string) string {
	// Walk up to find the .app bundle's Info.plist.
	dir := filepath.Dir(jt9Path)
	for i := 0; i < 4 && dir != "/" && dir != "."; i++ {
		plist := filepath.Join(dir, "Info.plist")
		if _, err := os.Stat(plist); err == nil {
			out, err := exec.Command("defaults", "read",
				strings.TrimSuffix(plist, ".plist"), "CFBundleShortVersionString").Output()
			if err == nil {
				v := strings.TrimSpace(string(out))
				if v != "" {
					return v
				}
			}
		}
		dir = filepath.Dir(dir)
	}
	return "unknown"
}
