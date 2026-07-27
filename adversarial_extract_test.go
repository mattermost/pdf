//go:build adversarial

package pdf

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Desired-behavior tests for cancellation / allocation bounds.
//
//	go run ./tools/gen_adversarial_pdfs -o tools/adversarial_pdfs --scale medium
//	go test -tags=adversarial -count=1 -v -run Adversarial .
//
// Failures mean the implementation does not yet meet the contract for that
// fixture (a real gap). Unexpected passes mean the fixture/test missed the
// predicted path, or the prediction was wrong.

func adversarialDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("ADVERSARIAL_PDF_DIR")
	if dir == "" {
		dir = filepath.Join("tools", "adversarial_pdfs")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("missing fixtures dir %s (run tools/gen_adversarial_pdfs.py first): %v", dir, err)
	}
	return dir
}

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	scale := os.Getenv("ADVERSARIAL_PDF_SCALE")
	if scale == "" {
		scale = "medium"
	}
	path := filepath.Join(adversarialDir(t), name+"."+scale+".pdf")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func extractWithTimeout(t *testing.T, data []byte, timeout time.Duration) (elapsed time.Duration, totalAlloc uint64, err error) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), timeout)
		defer cancel()
	}

	start := time.Now()
	out, err := r.GetPlainText(ctx)
	elapsed = time.Since(start)
	if out != nil {
		_, readErr := io.Copy(io.Discard, out)
		if err == nil {
			err = readErr
		}
	}

	runtime.GC()
	runtime.ReadMemStats(&after)
	totalAlloc = after.TotalAlloc - before.TotalAlloc
	return elapsed, totalAlloc, err
}

func requireCanceledCheaply(t *testing.T, label string, elapsed time.Duration, alloc uint64, err error, maxElapsed time.Duration, maxAlloc uint64) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("%s: want cancel/deadline, got err=%v elapsed=%s alloc=%d", label, err, elapsed, alloc)
	}
	if elapsed > maxElapsed {
		t.Fatalf("%s: cancel too slow: elapsed=%s (want <= %s) alloc=%d", label, elapsed, maxElapsed, alloc)
	}
	if alloc > maxAlloc {
		t.Fatalf("%s: cancel allocated too much: alloc=%d (want <= %d) elapsed=%s", label, alloc, maxAlloc, elapsed)
	}
	t.Logf("%s: elapsed=%s alloc=%d err=%v", label, elapsed, alloc, err)
}

func TestAdversarial_ManyOperatorsCancelsCheaply(t *testing.T) {
	data := loadFixture(t, "many_operators")
	elapsed, alloc, err := extractWithTimeout(t, data, 200*time.Microsecond)
	requireCanceledCheaply(t, "many_operators", elapsed, alloc, err, 5*time.Millisecond, 8<<20)
}

func TestAdversarial_FlateBombCancelsCheaply(t *testing.T) {
	data := loadFixture(t, "flate_bomb_tj")
	elapsed, alloc, err := extractWithTimeout(t, data, 200*time.Microsecond)
	requireCanceledCheaply(t, "flate_bomb_tj", elapsed, alloc, err, 5*time.Millisecond, 8<<20)
}

func TestAdversarial_HugeLiteralCancelsCheaply(t *testing.T) {
	data := loadFixture(t, "huge_literal_tj")
	elapsed, alloc, err := extractWithTimeout(t, data, 200*time.Microsecond)
	requireCanceledCheaply(t, "huge_literal_tj", elapsed, alloc, err, 5*time.Millisecond, 8<<20)
}

func TestAdversarial_ToUnicodeCmapCancelsCheaply(t *testing.T) {
	data := loadFixture(t, "tounicode_cmap")
	elapsed, alloc, err := extractWithTimeout(t, data, 200*time.Microsecond)
	requireCanceledCheaply(t, "tounicode_cmap", elapsed, alloc, err, 5*time.Millisecond, 8<<20)
}

func TestAdversarial_PredictorColumnsBounded(t *testing.T) {
	data := loadFixture(t, "predictor_columns")
	elapsed, alloc, err := extractWithTimeout(t, data, 200*time.Microsecond)
	// Wanted: reject/cap huge Columns (or honor ctx) without allocating ~2×Columns.
	if alloc > 8<<20 {
		t.Fatalf("predictor_columns: allocated %d (want <= 8MiB); err=%v elapsed=%s", alloc, err, elapsed)
	}
	if elapsed > 5*time.Millisecond {
		t.Fatalf("predictor_columns: completed too slowly: elapsed=%s (want <= 5ms); err=%v alloc=%d", elapsed, err, alloc)
	}
	t.Logf("predictor_columns: elapsed=%s alloc=%d err=%v", elapsed, alloc, err)
}

func TestAdversarial_AcroFormStaysCheap(t *testing.T) {
	data := loadFixture(t, "acroform_fields")
	elapsed, alloc, err := extractWithTimeout(t, data, 2*time.Second)
	if err != nil {
		t.Fatalf("GetPlainText: %v", err)
	}
	// Allow up to file size + 2 MiB: xref-table parsing is O(object count)
	// so the threshold must scale with the fixture, not be a fixed constant.
	maxAlloc := uint64(len(data)) + 2*(1<<20)
	if alloc > maxAlloc {
		t.Fatalf("acroform_fields: unexpectedly expensive alloc=%d (want <= %d) elapsed=%s", alloc, maxAlloc, elapsed)
	}
	t.Logf("acroform_fields: elapsed=%s alloc=%d", elapsed, alloc)
}

func TestAdversarial_NestedContentHitsDepthLimit(t *testing.T) {
	data := loadFixture(t, "nested_content")
	_, _, err := extractWithTimeout(t, data, 2*time.Second)
	if !errors.Is(err, errObjectNestingDepth) {
		t.Fatalf("nested_content: want nesting-depth error, got %v", err)
	}
	t.Logf("nested_content: err=%v", err)
}
