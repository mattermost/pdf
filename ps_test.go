package pdf

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestInterpretContinuesTokensAcrossContentStreams(t *testing.T) {
	pdfData := splitTextArrayPDF()
	reader, err := NewReader(bytes.NewReader(pdfData), int64(len(pdfData)))
	if err != nil {
		t.Fatal(err)
	}

	rd, err := reader.GetPlainText(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	text, err := io.ReadAll(rd)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(text)); got != "Hello world" {
		t.Fatalf("text = %q, want %q", got, "Hello world")
	}
}

// TestInterpretSeparatesAdjacentStreamsWithoutWhitespace verifies that
// Interpret does not merge tokens across a content-stream array boundary.
// The first stream ends with "10" (no trailing whitespace) and the second
// starts with "20" (no leading whitespace); without a separator inserted
// between the two streams, the lexer would read "1020" as a single number.
func TestInterpretSeparatesAdjacentStreamsWithoutWhitespace(t *testing.T) {
	pdfData := adjacentNumberStreamsPDF()
	reader, err := NewReader(bytes.NewReader(pdfData), int64(len(pdfData)))
	if err != nil {
		t.Fatal(err)
	}

	contents := reader.Page(1).V.Key("Contents")
	var gotOp string
	var gotArgs []float64
	err = Interpret(context.Background(), contents, func(stk *Stack, op string) {
		gotOp = op
		for stk.Len() > 0 {
			gotArgs = append([]float64{stk.Pop().Float64()}, gotArgs...)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotOp != "m" {
		t.Fatalf("op = %q, want %q", gotOp, "m")
	}
	if len(gotArgs) != 2 || gotArgs[0] != 10 || gotArgs[1] != 20 {
		t.Fatalf("args = %v, want [10 20]", gotArgs)
	}
}

// TestInterpretEmptyContentsArray verifies that Interpret does not panic on a
// page whose /Contents is an empty array. Before the capacity fix, an empty
// array made the reader slice's capacity negative (2*0-1 == -1), and make()
// panicked.
func TestInterpretEmptyContentsArray(t *testing.T) {
	pdfData := emptyContentsArrayPDF()
	reader, err := NewReader(bytes.NewReader(pdfData), int64(len(pdfData)))
	if err != nil {
		t.Fatal(err)
	}

	contents := reader.Page(1).V.Key("Contents")
	called := false
	err = Interpret(context.Background(), contents, func(stk *Stack, op string) {
		called = true
	})
	if err != nil {
		t.Fatalf("Interpret: %v", err)
	}
	if called {
		t.Fatal("expected no operators from an empty /Contents array")
	}
}

func emptyContentsArrayPDF() []byte {
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 4)
	writeObject := func(number int, body string) {
		offsets[number] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", number, body)
	}

	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << >> /Contents [] >>")

	xrefOffset := pdf.Len()
	pdf.WriteString("xref\n0 4\n0000000000 65535 f \n")
	for number := 1; number <= 3; number++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[number])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size 4 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset)
	return pdf.Bytes()
}

// TestInterpretCanceledContextSkipsStreamInitialization verifies that a
// canceled context stops Interpret before it initializes any of the array's
// stream readers. Before the ctx check was added to this loop, Interpret
// called strm.Index(i).Reader() for every stream up front, which runs that
// stream's decode filters immediately; on an already-canceled context that
// work (and any cost or panic it triggers) should never happen.
func TestInterpretCanceledContextSkipsStreamInitialization(t *testing.T) {
	pdfData := unsupportedFilterContentsArrayPDF()
	reader, err := NewReader(bytes.NewReader(pdfData), int64(len(pdfData)))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	contents := reader.Page(1).V.Key("Contents")
	err = Interpret(ctx, contents, func(stk *Stack, op string) {
		t.Fatal("operator called on a canceled context")
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func unsupportedFilterContentsArrayPDF() []byte {
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 5)
	writeObject := func(number int, body string) {
		offsets[number] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", number, body)
	}

	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << >> /Contents [4 0 R] >>")
	// /Filter /BogusFilter panics in applyFilter if .Reader() is ever
	// called on this stream, so its presence here proves whether Interpret
	// reached stream initialization or bailed out on ctx first.
	writeObject(4, "<< /Length 1 /Filter /BogusFilter >>\nstream\nx\nendstream")

	xrefOffset := pdf.Len()
	pdf.WriteString("xref\n0 5\n0000000000 65535 f \n")
	for number := 1; number <= 4; number++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[number])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size 5 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset)
	return pdf.Bytes()
}

func adjacentNumberStreamsPDF() []byte {
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 6)
	writeObject := func(number int, body string) {
		offsets[number] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", number, body)
	}

	// The writeStream helper in splitTextArrayPDF below sets each stream's
	// /Length one byte past len(content), which captures the "\n" written
	// between the content and "endstream" as trailing stream content. That
	// would mask the bug under test by giving every stream an implicit
	// trailing whitespace byte, so these two streams set their own exact
	// /Length instead, leaving no whitespace on either side of the boundary.
	writeExactStream := func(number int, content string) {
		writeObject(number, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	}

	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << >> /Contents [4 0 R 5 0 R] >>")
	writeExactStream(4, "10")
	writeExactStream(5, "20 m")

	xrefOffset := pdf.Len()
	pdf.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for number := 1; number <= 5; number++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[number])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset)
	return pdf.Bytes()
}

func splitTextArrayPDF() []byte {
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, 7)
	writeObject := func(number int, body string) {
		offsets[number] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", number, body)
	}
	writeStream := func(number int, content string) {
		writeObject(number, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content)+1, content))
	}

	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /F1 6 0 R >> >> /Contents [4 0 R 5 0 R] >>")
	writeStream(4, "BT /F1 12 Tf 20 100 Td [(Hello)")
	writeStream(5, "( world)] TJ ET")
	writeObject(6, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	xrefOffset := pdf.Len()
	pdf.WriteString("xref\n0 7\n0000000000 65535 f \n")
	for number := 1; number <= 6; number++ {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offsets[number])
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size 7 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xrefOffset)
	return pdf.Bytes()
}
