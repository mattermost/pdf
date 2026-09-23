package pdf

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestUnterminatedHexStringTerminates verifies that a content stream ending
// inside a hex string terminates on its own. readByte returns '\n' at EOF and
// readHexString skips whitespace, so it spins until the context deadline;
// GetTextByRow and GetTextByColumn use context.Background() and would never
// return.
func TestUnterminatedHexStringTerminates(t *testing.T) {
	_, err := plainText(t, "5 0 R", "BT /F1 12 Tf <00ab")
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("GetPlainText spun until the deadline on an unterminated hex string")
	}
}

// TestUnterminatedArrayTerminates verifies that text extraction terminates
// on a PDF whose content stream is truncated inside an unterminated array.
// An unclosed array at EOF must terminate, not loop appending io.EOF.
func TestUnterminatedArrayTerminates(t *testing.T) {
	r := newPageReader(t, "/Resources << >> /Contents 4 0 R", streamObj("BT /F1 12 Tf [ (hello) 1 2"))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := r.GetPlainText(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("GetPlainText: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetPlainText did not return within 5s: readArray is looping on io.EOF at end of a truncated content stream")
	}
}
