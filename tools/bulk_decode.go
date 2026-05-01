package main
import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"time"
	"github.com/kyleomalley/nocordhf/lib/ft8"
	"github.com/kyleomalley/nocordhf/lib/logging"
)
func main() {
	logging.InitFile(false, "t", "/dev/null")
	ft8.SetDecodeBudget(30 * time.Second)
	
	fmt.Printf("Processing %d files\n", len(os.Args)-1)
	
	for _, wavPath := range os.Args[1:] {
		data, err := os.ReadFile(wavPath)
		if err != nil {
			fmt.Printf("Error reading %s: %v\n", wavPath, err)
			continue
		}
		if len(data) < 44 {
			fmt.Printf("Skipping %s: too small\n", wavPath)
			continue
		}
		reader := bytes.NewReader(data[44:])
		var samples []float32
		for {
			var v int16
			if binary.Read(reader, binary.LittleEndian, &v) != nil { break }
			samples = append(samples, float32(v)/32768.0)
		}
		if len(samples) < 100000 {
			fmt.Printf("Skipping %s: insufficient samples (%d)\n", wavPath, len(samples))
			continue
		}
		
		name := wavPath[strings.LastIndex(wavPath, "/")+1:]
		outfile := fmt.Sprintf("/tmp/noc_%s.txt", strings.TrimSuffix(name, ".wav"))
		f, err := os.Create(outfile)
		if err != nil {
			fmt.Printf("Error creating %s: %v\n", outfile, err)
			continue
		}
		
		results := ft8.Decode(samples, time.Now().UTC(), nil)
		count := 0
		for _, r := range results {
			if r.Message.Text != "" {
				fmt.Fprintf(f, "%s\n", r.Message.Text)
				count++
			}
		}
		f.Close()
		fmt.Printf("Decoded %s: %d messages -> %s\n", name, count, outfile)
	}
}
