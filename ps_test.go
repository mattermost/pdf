package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// buildPagePDF writes a one-page PDF: objects 1-3 are the catalog, page tree
// and page (with the extra page dict keys in page), and objs are objects 4..
func buildPagePDF(page string, objs ...string) []byte {
	objs = append([]string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] " + page + " >>",
	}, objs...)

	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, obj := range objs {
		offsets[i] = pdf.Len()
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xrefOffset := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xrefOffset)
	return pdf.Bytes()
}

// newPageReader opens the PDF built by buildPagePDF.
func newPageReader(t *testing.T, page string, objs ...string) *Reader {
	t.Helper()
	pdf := buildPagePDF(page, objs...)
	r, err := NewReader(bytes.NewReader(pdf), int64(len(pdf)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// plainText extracts a one-page PDF whose /Contents is contents, with /F1
// (Helvetica) as object 4 and streams as objects 5.. The deadline turns a
// lexer hang into a test failure instead of a stuck test binary.
func plainText(t *testing.T, contents string, streams ...string) (string, error) {
	t.Helper()
	objs := []string{"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	for _, content := range streams {
		objs = append(objs, streamObj(content))
	}
	reader := newPageReader(t, "/Resources << /Font << /F1 4 0 R >> >> /Contents "+contents, objs...)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rd, err := reader.GetPlainText(ctx)
	if err != nil {
		return "", err
	}
	text, err := io.ReadAll(rd)
	return string(text), err
}

// interpret runs Interpret, turning a panic into a failure of this test
// instead of a crash of the whole test binary.
func interpret(t *testing.T, ctx context.Context, strm Value, do func(stk *Stack, op string)) error {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Interpret panicked: %v", r)
		}
	}()
	return Interpret(ctx, strm, do)
}

// bogusFilterStream panics in applyFilter if .Reader() is ever called on it,
// so reaching it proves Interpret opened that stream.
const bogusFilterStream = "<< /Length 1 /Filter /BogusFilter >>\nstream\nx\nendstream"

// streamObj returns a stream object whose /Length covers exactly content, so
// no stray byte before "endstream" leaks into the stream.
func streamObj(content string) string {
	return fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)
}

func TestInterpretContinuesTokensAcrossContentStreams(t *testing.T) {
	reader := newPageReader(t, "/Resources << /Font << /F1 6 0 R >> >> /Contents [4 0 R 5 0 R]",
		streamObj("BT /F1 12 Tf 20 100 Td [(Hello)"),
		streamObj("( world)] TJ ET"),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	)

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
// starts with "20" (no leading whitespace); unless each stream ends at its
// own EOF, the lexer would read "1020" as a single number.
func TestInterpretSeparatesAdjacentStreamsWithoutWhitespace(t *testing.T) {
	reader := newPageReader(t, "/Resources << >> /Contents [4 0 R 5 0 R]",
		streamObj("10"),
		streamObj("20 m"),
	)

	contents := reader.Page(1).V.Key("Contents")
	var gotOp string
	var gotArgs []float64
	err := Interpret(context.Background(), contents, func(stk *Stack, op string) {
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
// page whose /Contents is an empty array.
func TestInterpretEmptyContentsArray(t *testing.T) {
	reader := newPageReader(t, "/Resources << >> /Contents []")

	contents := reader.Page(1).V.Key("Contents")
	called := false
	err := Interpret(context.Background(), contents, func(stk *Stack, op string) {
		called = true
	})
	if err != nil {
		t.Fatalf("Interpret: %v", err)
	}
	if called {
		t.Fatal("expected no operators from an empty /Contents array")
	}
}

// TestInterpretOpensArrayStreamsLazily verifies that Interpret opens each
// stream of a /Contents array only once the previous one is exhausted, so a
// cancellation raised while parsing (request timeout, output limit) stops it
// before later streams allocate their decode state.
func TestInterpretOpensArrayStreamsLazily(t *testing.T) {
	reader := newPageReader(t, "/Resources << >> /Contents [4 0 R 5 0 R]",
		// The trailing newline ends the "q" token inside stream 4, so the
		// lexer never needs to read into stream 5 to dispatch it.
		streamObj("q\n"),
		bogusFilterStream,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	contents := reader.Page(1).V.Key("Contents")
	var ops []string
	err := Interpret(ctx, contents, func(stk *Stack, op string) {
		ops = append(ops, op)
		cancel()
	})
	if err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(ops) != 1 || ops[0] != "q" {
		t.Fatalf("ops = %v, want [q]", ops)
	}
}

// TestInterpretStreamBoundaries verifies that a malformed tail in one
// /Contents stream, or an entry that isn't a stream, does not lose the text
// of the surrounding streams. The spec only allows stream breaks at token
// boundaries, so comments, strings and arrays left open at the end of a
// stream must not swallow the next one.
func TestInterpretStreamBoundaries(t *testing.T) {
	const a, b = "BT /F1 12 Tf (A) Tj ET", "BT /F1 12 Tf (B) Tj ET"
	tests := []struct {
		name     string
		contents string
		streams  []string
	}{
		{"comment without EOL", "[5 0 R 6 0 R]", []string{a + " % note", b}},
		{"unterminated string", "[5 0 R 6 0 R]", []string{a + " (x", b}},
		{"unclosed array", "[5 0 R 6 0 R]", []string{a + " [ (x)", b}},
		{"null entry", "[5 0 R null 6 0 R]", []string{a, b}},
		{"dangling reference", "[5 0 R 99 0 R 6 0 R]", []string{a, b}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, err := plainText(t, tt.contents, tt.streams...)
			if err != nil {
				t.Fatalf("GetPlainText: %v", err)
			}
			if !strings.Contains(text, "A") || !strings.Contains(text, "B") {
				t.Fatalf("text = %q, want both A and B", text)
			}
		})
	}
}

// TestInterpretPreCanceledContext verifies that Interpret returns
// context.Canceled without opening any stream when ctx is already canceled.
func TestInterpretPreCanceledContext(t *testing.T) {
	reader := newPageReader(t, "/Resources << >> /Contents [4 0 R]", bogusFilterStream)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := interpret(t, ctx, reader.Page(1).V.Key("Contents"), func(stk *Stack, op string) {
		t.Fatal("operator called on a canceled context")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestInterpretContinuesDictAcrossContentStreams verifies that a dict operand
// split across /Contents streams, at any token boundary, parses as one dict.
func TestInterpretContinuesDictAcrossContentStreams(t *testing.T) {
	tests := []struct{ name, first, second string }{
		{"between entries", "/P << /MCID 0", "/K 1 >> BDC"},
		{"between key and value", "/P << /MCID", "0 /K 1 >> BDC"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := newPageReader(t, "/Resources << >> /Contents [4 0 R 5 0 R]", streamObj(tt.first), streamObj(tt.second))
			var props Value
			err := interpret(t, context.Background(), reader.Page(1).V.Key("Contents"), func(stk *Stack, op string) {
				if op == "BDC" {
					props = stk.Pop()
				}
			})
			if err != nil {
				t.Fatalf("Interpret: %v", err)
			}
			if props.Kind() != Dict || props.Key("MCID").Int64() != 0 || props.Key("K").Int64() != 1 || len(props.Keys()) != 2 {
				t.Fatalf("BDC properties = %v, want << /MCID 0 /K 1 >>", props)
			}
		})
	}
}

// cancelOnReadAt cancels a context the first time offset off is read, so a
// test can cancel at an exact byte of the file instead of racing a timer.
type cancelOnReadAt struct {
	r      io.ReaderAt
	off    int64
	cancel context.CancelFunc
}

func (c *cancelOnReadAt) ReadAt(p []byte, off int64) (int, error) {
	if off == c.off {
		c.cancel()
	}
	return c.r.ReadAt(p, off)
}

// TestInterpretReturnsCancelDuringTokenRead verifies that a cancellation
// landing while the lexer is mid-token is returned by Interpret as an error,
// not raised as a panic, and that the next stream is never opened. Stream 4
// is a bare "q", so the lexer must read past the stream boundary to finish
// the token, and ctx is canceled as stream 4's bytes are read.
func TestInterpretReturnsCancelDuringTokenRead(t *testing.T) {
	pdf := buildPagePDF("/Resources << >> /Contents [4 0 R 5 0 R]", streamObj("q"), bogusFilterStream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trigger := &cancelOnReadAt{
		r:      bytes.NewReader(pdf),
		off:    int64(bytes.Index(pdf, []byte("stream\nq\n")) + len("stream\n")),
		cancel: cancel,
	}
	reader, err := NewReader(trigger, int64(len(pdf)))
	if err != nil {
		t.Fatal(err)
	}

	err = interpret(t, ctx, reader.Page(1).V.Key("Contents"), func(stk *Stack, op string) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
