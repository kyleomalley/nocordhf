// compare_corpus diffs two decode corpora (typically nocordhf vs jt9) and
// emits a markdown report with per-slot and aggregate recall, precision, and
// F1. The reference (jt9) corpus is the ground truth; the candidate
// (nocordhf) corpus is what we're scoring.
//
// Usage:
//
//	compare_corpus -ref recordings/corpus/jt9/v2.8.0 \
//	               -cand recordings/corpus/nocordhf/<sha> \
//	               [-out report.md]
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// jt9 appends decode-pass markers (`a1`, `a2`, `q0`, `?`) outside the
// 22-char FT8 message field, and a single line can carry more than one
// (`... ? a1`). They aren't part of the message and would cause spurious
// mismatches against nocordhf, so we strip them all before comparison.
var jt9MarkerRE = regexp.MustCompile(`(\s+(a\d+|q\d+|\?))+\s*$`)

// FT8 signal reports are zero-padded to two digits by jt9 (`-07`, `R+02`)
// but emitted bare by nocordhf (`-7`, `R+2`). Match any non-digit context
// (a space or `R`) followed by sign + leading-zero digit, and drop the
// zero. Go's RE2 has no lookbehind so the prefix character is captured
// and replayed in the replacement.
var sigReportRE = regexp.MustCompile(`([^0-9])([+-])0(\d)\b`)

func normalize(msg string) string {
	msg = jt9MarkerRE.ReplaceAllString(msg, "")
	msg = sigReportRE.ReplaceAllString(msg, "${1}${2}${3}")
	return strings.TrimSpace(msg)
}

func main() {
	refDir := flag.String("ref", "", "reference corpus directory (e.g. jt9)")
	candDir := flag.String("cand", "", "candidate corpus directory (e.g. nocordhf)")
	outPath := flag.String("out", "", "report output file (default: stdout)")
	flag.Parse()

	if *refDir == "" || *candDir == "" {
		fmt.Fprintf(os.Stderr, "usage: compare_corpus -ref DIR -cand DIR [-out FILE]\n")
		os.Exit(1)
	}

	refSlots, refHeader := readCorpus(*refDir)
	candSlots, candHeader := readCorpus(*candDir)

	// Score only slots present in both corpora — counting a slot the
	// candidate never ran as "0 of N missed" would dilute recall and hide
	// the real per-slot performance. Slots present in only one side are
	// reported separately so the partial-coverage case is obvious.
	var common, refOnlySlots, candOnlySlots []string
	for s := range refSlots {
		if _, ok := candSlots[s]; ok {
			common = append(common, s)
		} else {
			refOnlySlots = append(refOnlySlots, s)
		}
	}
	for s := range candSlots {
		if _, ok := refSlots[s]; !ok {
			candOnlySlots = append(candOnlySlots, s)
		}
	}
	sort.Strings(common)
	sort.Strings(refOnlySlots)
	sort.Strings(candOnlySlots)

	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "create %s: %v\n", *outPath, err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}

	fmt.Fprintf(out, "# Corpus Comparison\n\n")
	fmt.Fprintf(out, "- **Reference:** `%s` — %s\n", *refDir, refHeader)
	fmt.Fprintf(out, "- **Candidate:** `%s` — %s\n\n", *candDir, candHeader)

	fmt.Fprintf(out, "## Per-slot results\n\n")
	fmt.Fprintf(out, "| Slot | ref | cand | match | recall | prec |\n")
	fmt.Fprintf(out, "|------|-----|------|-------|--------|------|\n")

	var totalRef, totalCand, totalMatch int
	missedAll := make(map[string]int) // ref-only message → count of slots
	extraAll := make(map[string]int)  // cand-only message → count of slots

	if len(refOnlySlots) > 0 || len(candOnlySlots) > 0 {
		fmt.Fprintf(out, "> Note: partial coverage — %d slot(s) only in ref, %d slot(s) only in cand. Aggregate metrics use the %d shared slot(s).\n\n",
			len(refOnlySlots), len(candOnlySlots), len(common))
	}

	for _, slot := range common {
		ref := refSlots[slot]
		cand := candSlots[slot]
		match, refOnly, candOnly := setOps(ref, cand)

		totalRef += len(ref)
		totalCand += len(cand)
		totalMatch += match

		for _, m := range refOnly {
			missedAll[m]++
		}
		for _, m := range candOnly {
			extraAll[m]++
		}

		recall := pct(match, len(ref))
		prec := pct(match, len(cand))
		fmt.Fprintf(out, "| %s | %d | %d | %d | %s | %s |\n",
			shortSlotName(slot), len(ref), len(cand), match, recall, prec)
	}

	fmt.Fprintf(out, "\n## Aggregate\n\n")
	recall := float64(totalMatch) / float64(max(totalRef, 1))
	prec := float64(totalMatch) / float64(max(totalCand, 1))
	f1 := 0.0
	if recall+prec > 0 {
		f1 = 2 * recall * prec / (recall + prec)
	}
	fmt.Fprintf(out, "- Reference total: **%d** unique messages\n", totalRef)
	fmt.Fprintf(out, "- Candidate total: **%d** unique messages\n", totalCand)
	fmt.Fprintf(out, "- Matched:         **%d**\n", totalMatch)
	fmt.Fprintf(out, "- Recall:          **%.1f%%**\n", recall*100)
	fmt.Fprintf(out, "- Precision:       **%.1f%%**\n", prec*100)
	fmt.Fprintf(out, "- F1 score:        **%.1f%%**\n", f1*100)

	fmt.Fprintf(out, "\n## Top missed (in ref, not in cand)\n\n")
	writeTop(out, missedAll, 20)

	fmt.Fprintf(out, "\n## Top extras (in cand, not in ref)\n\n")
	writeTop(out, extraAll, 20)

	if *outPath != "" {
		fmt.Printf("Wrote report to %s\n", *outPath)
		fmt.Printf("Recall %.1f%%  Precision %.1f%%  F1 %.1f%%\n",
			recall*100, prec*100, f1*100)
	}
}

// readCorpus loads every *.txt slot file from dir, returning a map of slot
// basename → set of messages, plus the corpus header line (build/version
// metadata) used to label the report.
func readCorpus(dir string) (map[string]map[string]struct{}, string) {
	slots := make(map[string]map[string]struct{})
	var header string

	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", dir, err)
		os.Exit(1)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".txt")
		msgs := make(map[string]struct{})
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "#") {
				if header == "" && !strings.HasPrefix(line, "# freq") {
					header = strings.TrimPrefix(line, "# ")
				}
				continue
			}
			// Format: "freq\tsnr\tmessage" — the message is everything
			// after the second tab so we don't truncate at spaces.
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) < 3 {
				continue
			}
			msgs[normalize(parts[2])] = struct{}{}
		}
		f.Close()
		slots[base] = msgs
	}
	return slots, header
}

func setOps(ref, cand map[string]struct{}) (int, []string, []string) {
	var match int
	var refOnly, candOnly []string
	for m := range ref {
		if _, ok := cand[m]; ok {
			match++
		} else {
			refOnly = append(refOnly, m)
		}
	}
	for m := range cand {
		if _, ok := ref[m]; !ok {
			candOnly = append(candOnly, m)
		}
	}
	return match, refOnly, candOnly
}

func pct(num, denom int) string {
	if denom == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(num)/float64(denom))
}

// shortSlotName trims the long recorder-build suffix off a recording slot
// name so the report table stays readable. e.g.
// "ft8_20260501_034030_5bf10a9-20260501034017_0" → "034030".
func shortSlotName(s string) string {
	parts := strings.Split(s, "_")
	if len(parts) >= 3 {
		return parts[2]
	}
	return s
}

func writeTop(out *os.File, counts map[string]int, limit int) {
	type kv struct {
		msg string
		n   int
	}
	items := make([]kv, 0, len(counts))
	for m, n := range counts {
		items = append(items, kv{m, n})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].n != items[j].n {
			return items[i].n > items[j].n
		}
		return items[i].msg < items[j].msg
	})
	if len(items) == 0 {
		fmt.Fprintf(out, "_(none)_\n")
		return
	}
	if len(items) > limit {
		items = items[:limit]
	}
	for _, it := range items {
		fmt.Fprintf(out, "- `%s` (%d slot%s)\n", it.msg, it.n, plural(it.n))
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
