// Probe GetPlainText against an adversarial PDF under an optional short deadline.
//
// Usage:
//
//	GOMEMLIMIT=256MiB go run ./tools/probe_extract.go [-timeout 2s] file.pdf
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/mattermost/pdf"
)

func main() {
	timeout := flag.Duration("timeout", 2*time.Second, "extraction deadline (0 = no deadline)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "usage: %s [-timeout 2s] file.pdf\n", os.Args[0])
		os.Exit(2)
	}
	path := flag.Arg(0)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	start := time.Now()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewReader: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	var cancel context.CancelFunc
	if *timeout > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), *timeout)
		defer cancel()
	}

	out, err := r.GetPlainText(ctx)
	elapsed := time.Since(start)

	var n int64
	if out != nil {
		var buf bytes.Buffer
		n, _ = buf.ReadFrom(out)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)

	fmt.Printf("file=%s size_on_disk=%d\n", path, len(data))
	fmt.Printf("elapsed=%s extract_err=%v plaintext_bytes=%d\n", elapsed, err, n)
	fmt.Printf("heap_alloc_delta=%d heap_inuse_delta=%d total_alloc_delta=%d\n",
		int64(after.HeapAlloc)-int64(before.HeapAlloc),
		int64(after.HeapInuse)-int64(before.HeapInuse),
		int64(after.TotalAlloc)-int64(before.TotalAlloc),
	)
	// Non-nil extract_err is still useful for gap probes (e.g. predictor
	// allocates, then the filtered reader errors). Exit 0 so scripts can
	// compare total_alloc_delta across fixtures.
}

