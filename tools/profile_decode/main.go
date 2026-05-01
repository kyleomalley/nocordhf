// profile_decode runs ft8.Decode on a single WAV with CPU profiling enabled
// so we can identify hot paths after the minScore=2 / no-cap changes.
//
//	go run ./tools/profile_decode <wav> [<out.prof>]
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"runtime/pprof"
	"time"

	"github.com/kyleomalley/nocordhf/lib/ft8"
	"github.com/kyleomalley/nocordhf/lib/logging"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: profile_decode <wav> [<out.prof>]")
		os.Exit(1)
	}
	wav := os.Args[1]
	out := "decode.prof"
	if len(os.Args) > 2 {
		out = os.Args[2]
	}

	logging.InitFile(false, "prof", "/dev/null")

	data, err := os.ReadFile(wav)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}
	var samples []float32
	r := bytes.NewReader(data[44:])
	for {
		var v int16
		if binary.Read(r, binary.LittleEndian, &v) != nil {
			break
		}
		samples = append(samples, float32(v)/32768.0)
	}

	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prof create: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()
	pprof.StartCPUProfile(f)
	defer pprof.StopCPUProfile()

	// Generous budget so we measure actual cost, not the budget cutoff.
	ft8.SetDecodeBudget(60 * time.Second)
	t0 := time.Now()
	results := ft8.Decode(samples, time.Now().UTC(), nil)
	fmt.Printf("decoded %d messages in %v\n", len(results), time.Since(t0))
	fmt.Printf("profile written to %s\n", out)
}
