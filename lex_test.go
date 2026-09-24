package pdf

import "testing"

// TestUnterminatedHexStringTerminates verifies that a content stream ending
// inside a hex string terminates on its own instead of relying on ctx:
// GetTextByRow and GetTextByColumn use context.Background().
func TestUnterminatedHexStringTerminates(t *testing.T) {
	if _, err := plainText(t, "5 0 R", "BT /F1 12 Tf <00ab"); err != nil {
		t.Fatalf("GetPlainText: %v", err)
	}
}

// TestUnterminatedArrayTerminates verifies that text extraction terminates
// on a PDF whose content stream is truncated inside an unterminated array.
// An unclosed array at EOF must terminate, not loop appending io.EOF.
func TestUnterminatedArrayTerminates(t *testing.T) {
	if _, err := plainText(t, "5 0 R", "BT /F1 12 Tf [ (hello) 1 2"); err != nil {
		t.Fatalf("GetPlainText: %v", err)
	}
}
