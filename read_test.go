package pdf

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/ascii85"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReadObjectMaxDepth(t *testing.T) {
	// Craft a payload of deeply nested "0 0 obj" sequences that would
	// previously cause unbounded recursion and a fatal stack overflow.
	// With the depth limit in place this must produce a recoverable panic
	// (caught here) instead of crashing the process.
	const depth = maxObjectDepth + 100
	var payload bytes.Buffer
	for i := 0; i < depth; i++ {
		payload.WriteString("0 0 obj\n")
	}

	b := newBuffer(&payload, 0)
	b.allowEOF = true

	panicked := false
	var panicMsg string
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				panicMsg = r.(error).Error()
			}
		}()
		b.readObject()
	}()

	if !panicked {
		t.Fatal("expected panic from deeply nested objects, but readObject returned normally")
	}
	if !strings.Contains(panicMsg, "maximum depth") {
		t.Fatalf("expected 'maximum depth' in panic message, got: %s", panicMsg)
	}
}

func TestReadDictMaxDepth(t *testing.T) {
	// Deeply nested dictionaries: << /A << /A << ... >> >> >>
	var payload bytes.Buffer
	const depth = maxObjectDepth + 100
	for i := 0; i < depth; i++ {
		payload.WriteString("<< /A ")
	}
	payload.WriteString("null")
	for i := 0; i < depth; i++ {
		payload.WriteString(" >>")
	}

	b := newBuffer(&payload, 0)
	b.allowEOF = true

	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		b.readObject()
	}()

	if !panicked {
		t.Fatal("expected panic from deeply nested dicts, but readObject returned normally")
	}
}

func TestReadArrayMaxDepth(t *testing.T) {
	// Deeply nested arrays: [ [ [ ... ] ] ]
	var payload bytes.Buffer
	const depth = maxObjectDepth + 100
	for i := 0; i < depth; i++ {
		payload.WriteString("[ ")
	}
	payload.WriteString("null")
	for i := 0; i < depth; i++ {
		payload.WriteString(" ]")
	}

	b := newBuffer(&payload, 0)
	b.allowEOF = true

	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		b.readObject()
	}()

	if !panicked {
		t.Fatal("expected panic from deeply nested arrays, but readObject returned normally")
	}
}

func TestReadObjectNormalDepth(t *testing.T) {
	// A moderately nested structure should parse without hitting the limit.
	var payload bytes.Buffer
	const depth = 50
	for i := 0; i < depth; i++ {
		payload.WriteString("<< /A ")
	}
	payload.WriteString("(hello)")
	for i := 0; i < depth; i++ {
		payload.WriteString(" >>")
	}

	b := newBuffer(&payload, 0)
	b.allowEOF = true

	panicked := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
			}
		}()
		obj := b.readObject()
		if obj == nil {
			t.Fatal("expected non-nil object from moderately nested dict")
		}
	}()

	if panicked {
		t.Fatal("unexpected panic from moderately nested dicts")
	}
}

func TestNewReaderMaliciousPDF(t *testing.T) {
	// Reproduce the exact attack vector from MM-63434: a PDF with millions
	// of "0 0 obj" tokens that triggers deep recursion during NewReader's
	// xref parsing. With the fix, NewReader should return an error (via
	// recovered panic) rather than crashing the process.
	var pdf bytes.Buffer
	pdf.WriteString("%PDF-1.0\n")
	for i := 0; i < 10_000; i++ {
		pdf.WriteString("0\n0\nobj\n")
	}
	pdf.WriteString("startxref\n0\n%%EOF\n")

	data := pdf.Bytes()
	_, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err == nil {
		t.Fatal("expected error from malicious PDF, got nil")
	}
}

// testTimeout bounds every helper below so a parser that never returns fails
// the test instead of wedging the test binary.
const testTimeout = 10 * time.Second

// classicPDF assembles objects 1..len(objs)-1 (objs[0] is unused) into a PDF
// with a classic cross-reference table. Any "$XREF" in trailerExtra is
// replaced with the offset of that table.
func classicPDF(objs []string, trailerExtra string) []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i := 1; i < len(objs); i++ {
		offsets[i] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", i, objs[i])
	}
	xrefOffset := body.Len()
	fmt.Fprintf(&body, "xref\n0 %d\n0000000000 65535 f \n", len(objs))
	for i := 1; i < len(objs); i++ {
		fmt.Fprintf(&body, "%010d 00000 n \n", offsets[i])
	}
	extra := strings.ReplaceAll(trailerExtra, "$XREF", strconv.Itoa(xrefOffset))
	fmt.Fprintf(&body, "trailer\n<< /Size %d /Root 1 0 R%s >>\nstartxref\n%d\n%%%%EOF\n", len(objs), extra, xrefOffset)
	return body.Bytes()
}

// xrefStreamPDF is classicPDF with an xref stream instead of an xref table.
// An object listed in compressed has no body of its own: the entry gives the
// id of the object stream holding it and its index within that stream.
func xrefStreamPDF(objs []string, compressed map[int][2]int) []byte {
	var body bytes.Buffer
	body.WriteString("%PDF-1.5\n")
	offsets := make([]int, len(objs))
	for i := 1; i < len(objs); i++ {
		if objs[i] == "" {
			continue
		}
		offsets[i] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", i, objs[i])
	}

	var data bytes.Buffer
	put := func(kind, f2, f3 int) {
		data.WriteByte(byte(kind))
		binary.Write(&data, binary.BigEndian, uint32(f2))
		binary.Write(&data, binary.BigEndian, uint32(f3))
	}
	put(0, 0, 65535)
	for i := 1; i < len(objs); i++ {
		if c, ok := compressed[i]; ok {
			put(2, c[0], c[1])
		} else {
			put(1, offsets[i], 0)
		}
	}
	xrefID := len(objs)
	xrefOffset := body.Len()
	put(1, xrefOffset, 0)

	fmt.Fprintf(&body, "%d 0 obj\n<< /Type /XRef /Size %d /W [1 4 4] /Root 1 0 R /Length %d >>\nstream\n%s\nendstream\nendobj\n",
		xrefID, xrefID+1, data.Len(), data.String())
	fmt.Fprintf(&body, "startxref\n%d\n%%%%EOF\n", xrefOffset)
	return body.Bytes()
}

// objStm builds an object stream holding bodies[i] as object ids[i]. n
// overrides the declared /N when it is not negative, and pad widens the
// header with trailing spaces so /First is larger than the entries need.
func objStm(ids []int, bodies []string, n, pad int, extra string) string {
	var hdr, data bytes.Buffer
	for i, id := range ids {
		fmt.Fprintf(&hdr, "%d %d ", id, data.Len())
		data.WriteString(bodies[i] + " ")
	}
	hdr.WriteString(strings.Repeat(" ", pad))
	if n < 0 {
		n = len(ids)
	}
	content := hdr.String() + data.String()
	return fmt.Sprintf("<< /Type /ObjStm /N %d /First %d /Length %d%s >>\nstream\n%s\nendstream",
		n, hdr.Len(), len(content), extra, content)
}

// extract reads all the text out of data, reporting a panic as an error and
// a parser that never returns as a test failure.
func extract(t *testing.T, data []byte) (text string, err error) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				if e, ok := r.(error); ok {
					err = e
				} else {
					err = fmt.Errorf("%v", r)
				}
			}
		}()
		r, rerr := NewReader(bytes.NewReader(data), int64(len(data)))
		if rerr != nil {
			err = rerr
			return
		}
		rd, gerr := r.GetPlainText(context.Background())
		if gerr != nil {
			err = gerr
			return
		}
		b, rderr := io.ReadAll(rd)
		text, err = string(b), rderr
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("extraction did not finish")
	}
	return text, err
}

// withinTimeout runs f and fails the test if it does not return in time.
func withinTimeout(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("call did not finish")
	}
}

// pageObjs is the catalog, page tree and page of a one-page PDF whose
// content is object 4 and whose /F1 font is object 5.
var pageObjs = []string{
	"",
	"<< /Type /Catalog /Pages 2 0 R >>",
	"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
	"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
	"",
	"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
}

func TestXrefTablePrevRepeatedOffset(t *testing.T) {
	objs := append([]string{}, pageObjs...)
	objs[4] = streamObj("BT /F1 12 Tf (hi) Tj ET")
	data := classicPDF(objs, " /Prev $XREF")

	withinTimeout(t, func() {
		_, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err == nil || !strings.Contains(err.Error(), "Prev repeats offset") {
			t.Errorf("err = %v, want an xref Prev offset error", err)
		}
	})
}

func TestXrefTablePrevMutualCycle(t *testing.T) {
	objs := append([]string{}, pageObjs...)
	objs[4] = streamObj("BT /F1 12 Tf (hi) Tj ET")
	// The fixed-width placeholder is patched below without moving any offset.
	const placeholder = "0000000000"
	base := classicPDF(objs, " /Prev "+placeholder)
	firstXref := strings.LastIndex(string(base), "xref\n0 ")

	var pdf bytes.Buffer
	pdf.Write(base)
	xref2 := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 1\n0000000000 65535 f \n")
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R /Prev %d >>\nstartxref\n%d\n%%%%EOF\n",
		len(objs), firstXref, xref2)

	data := bytes.Replace(pdf.Bytes(), []byte("/Prev "+placeholder), []byte(fmt.Sprintf("/Prev %010d", xref2)), 1)

	withinTimeout(t, func() {
		_, err := NewReader(bytes.NewReader(data), int64(len(data)))
		if err == nil || !strings.Contains(err.Error(), "Prev repeats offset") {
			t.Errorf("err = %v, want an xref Prev offset error", err)
		}
	})
}

func TestXrefTableIncrementalUpdate(t *testing.T) {
	objs := append([]string{}, pageObjs...)
	objs[4] = streamObj("BT /F1 12 Tf (first) Tj ET")
	base := classicPDF(objs, "")
	firstXref := strings.LastIndex(string(base), "xref\n0 ")

	var pdf bytes.Buffer
	pdf.Write(base)
	updated := streamObj("BT /F1 12 Tf (second) Tj ET")
	offset := pdf.Len()
	fmt.Fprintf(&pdf, "4 0 obj\n%s\nendobj\n", updated)
	xref2 := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 1\n0000000000 65535 f \n4 1\n%010d 00000 n \n", offset)
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R /Prev %d >>\nstartxref\n%d\n%%%%EOF\n",
		len(objs), firstXref, xref2)

	text, err := extract(t, pdf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "second") {
		t.Fatalf("text = %q, want the updated content stream", text)
	}
}

func TestXrefStreamPrevRepeatedOffset(t *testing.T) {
	objs := append([]string{}, pageObjs...)
	objs[4] = streamObj("BT /F1 12 Tf (hi) Tj ET")
	data := xrefStreamPDF(objs, nil)

	// Point the xref stream's /Prev back at the xref stream itself.
	start := bytes.LastIndex(data, []byte("startxref\n"))
	off, err := strconv.Atoi(strings.TrimSpace(string(data[start+len("startxref\n") : bytes.LastIndex(data, []byte("\n%%EOF"))])))
	if err != nil {
		t.Fatal(err)
	}
	// The key goes inside the xref stream object, which starts at off, so no
	// offset recorded elsewhere in the file moves.
	withPrev := bytes.Replace(data, []byte("/Type /XRef"), []byte(fmt.Sprintf("/Prev %d /Type /XRef", off)), 1)

	withinTimeout(t, func() {
		_, err := NewReader(bytes.NewReader(withPrev), int64(len(withPrev)))
		if err == nil || !strings.Contains(err.Error(), "Prev repeats offset") {
			t.Errorf("err = %v, want an xref Prev offset error", err)
		}
	})
}

func TestObjectStreamMemberResolves(t *testing.T) {
	// The catalog and the page tree live in an object stream, the page tree
	// last, so the header scan has to walk past the first member.
	objs := []string{
		"", "", "",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 5 0 R /Resources << /Font << /F1 6 0 R >> >> >>",
		objStm([]int{1, 2},
			[]string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"}, -1, 0, ""),
		streamObj("BT /F1 12 Tf (members) Tj ET"),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	data := xrefStreamPDF(objs, map[int][2]int{1: {4, 0}, 2: {4, 1}})

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "members") {
		t.Fatalf("text = %q, want the page content", text)
	}
}

func TestObjectStreamIndirectLength(t *testing.T) {
	stm := objStm([]int{1, 2},
		[]string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"}, -1, 0, "")
	// Replace the direct /Length with a reference to a plain integer object.
	length := stm[strings.Index(stm, "/Length ")+len("/Length ") : strings.Index(stm, " >>\nstream")]
	stm = strings.Replace(stm, "/Length "+length, "/Length 7 0 R", 1)

	objs := []string{
		"", "", "",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 5 0 R /Resources << /Font << /F1 6 0 R >> >> >>",
		stm,
		streamObj("BT /F1 12 Tf (indirect) Tj ET"),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		length,
	}
	data := xrefStreamPDF(objs, map[int][2]int{1: {4, 0}, 2: {4, 1}})

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "indirect") {
		t.Fatalf("text = %q, want the page content", text)
	}
}

func TestObjectStreamLengthInsideAnotherObjectStream(t *testing.T) {
	stm := objStm([]int{1, 2},
		[]string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>"}, -1, 0, "")
	length := stm[strings.Index(stm, "/Length ")+len("/Length ") : strings.Index(stm, " >>\nstream")]
	stm = strings.Replace(stm, "/Length "+length, "/Length 7 0 R", 1)

	objs := []string{
		"", "", "",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 5 0 R /Resources << /Font << /F1 6 0 R >> >> >>",
		stm,
		streamObj("BT /F1 12 Tf (nested) Tj ET"),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		"", // object 7: the length, held in the object stream below
		objStm([]int{7}, []string{length}, -1, 0, ""),
	}
	data := xrefStreamPDF(objs, map[int][2]int{1: {4, 0}, 2: {4, 1}, 7: {8, 0}})

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "nested") {
		t.Fatalf("text = %q, want the page content", text)
	}
}

func TestObjectStreamLengthInsideItself(t *testing.T) {
	stm := objStm([]int{1, 7},
		[]string{"<< /Type /Catalog /Pages 2 0 R >>", "99"}, -1, 0, "")
	length := stm[strings.Index(stm, "/Length ")+len("/Length ") : strings.Index(stm, " >>\nstream")]
	stm = strings.Replace(stm, "/Length "+length, "/Length 7 0 R", 1)

	objs := []string{"", "", "<< /Type /Pages /Kids [] /Count 0 >>", "", stm, "", "", ""}
	data := xrefStreamPDF(objs, map[int][2]int{1: {4, 0}, 7: {4, 1}})

	_, err := extract(t, data)
	if err == nil || !strings.Contains(err.Error(), "nesting exceeds") {
		t.Fatalf("err = %v, want an object stream nesting error", err)
	}
}

func TestObjectStreamContainerIsItselfCompressed(t *testing.T) {
	inner := objStm([]int{1}, []string{"<< /Type /Catalog /Pages 2 0 R >>"}, -1, 0, "")
	objs := []string{
		"", "", "<< /Type /Pages /Kids [] /Count 0 >>", "",
		"", // object 4: an object stream that is itself stored in object 5
		objStm([]int{4}, []string{inner}, -1, 0, ""),
	}
	data := xrefStreamPDF(objs, map[int][2]int{1: {4, 0}, 4: {5, 0}})

	_, err := extract(t, data)
	if err == nil || !strings.Contains(err.Error(), "unusable container") {
		t.Fatalf("err = %v, want an unusable container error", err)
	}
}

func TestObjectStreamDeclaredCountBeyondHeader(t *testing.T) {
	objs := []string{
		"", "", "<< /Type /Pages /Kids [] /Count 0 >>", "",
		objStm([]int{9}, []string{"null"}, 1<<30, 0, ""),
	}
	data := xrefStreamPDF(objs, map[int][2]int{1: {4, 0}})

	_, err := extract(t, data)
	if err == nil || !strings.Contains(err.Error(), "declares") {
		t.Fatalf("err = %v, want a declared count error", err)
	}
}

func TestObjectStreamHeaderEndsEarly(t *testing.T) {
	// /N fits within /First, but the header holds a single pair and the
	// stream's own data follows. The scan must stop at the first token that
	// is not an integer instead of reading pairs out of the object data,
	// where "1 0" would look like an entry for object 1.
	objs := []string{
		"", "", "<< /Type /Pages /Kids [] /Count 0 >>", "",
		objStm([]int{9}, []string{"null null 1 0"}, 5, 20, ""),
	}
	data := xrefStreamPDF(objs, map[int][2]int{1: {4, 0}})

	_, err := extract(t, data)
	if err == nil || !strings.Contains(err.Error(), "cannot find object in stream") {
		t.Fatalf("err = %v, want a missing object error", err)
	}
}

func TestObjectStreamExtendsChainResolves(t *testing.T) {
	// Object 1 is declared in object stream 4 but actually stored in the
	// stream 4 extends.
	objs := []string{
		"", "", "",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents 6 0 R /Resources << /Font << /F1 7 0 R >> >> >>",
		objStm([]int{2}, []string{"<< /Type /Pages /Kids [3 0 R] /Count 1 >>"}, -1, 0, " /Extends 5 0 R"),
		objStm([]int{1}, []string{"<< /Type /Catalog /Pages 2 0 R >>"}, -1, 0, ""),
		streamObj("BT /F1 12 Tf (extends) Tj ET"),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	data := xrefStreamPDF(objs, map[int][2]int{1: {4, 0}, 2: {4, 0}})

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "extends") {
		t.Fatalf("text = %q, want the page content", text)
	}
}

func TestObjectStreamExtendsChainRepeats(t *testing.T) {
	objs := []string{
		"", "", "<< /Type /Pages /Kids [] /Count 0 >>", "",
		objStm([]int{9}, []string{"null"}, -1, 0, " /Extends 5 0 R"),
		objStm([]int{8}, []string{"null"}, -1, 0, " /Extends 4 0 R"),
	}
	data := xrefStreamPDF(objs, map[int][2]int{1: {4, 0}})

	_, err := extract(t, data)
	if err == nil || !strings.Contains(err.Error(), "extends itself") {
		t.Fatalf("err = %v, want an Extends chain error", err)
	}
}

// extendsChainPDF stores the catalog at the end of an acyclic Extends chain
// of hops links, while the xref names the first stream of the chain as its
// container.
func extendsChainPDF(hops int) []byte {
	first := 4
	content := first + hops + 1
	objs := []string{
		"", "",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", content, content+1),
	}
	for i := 0; i < hops; i++ {
		objs = append(objs, objStm([]int{100 + i}, []string{"null"}, -1, 0, fmt.Sprintf(" /Extends %d 0 R", first+i+1)))
	}
	objs = append(objs,
		objStm([]int{1}, []string{"<< /Type /Catalog /Pages 2 0 R >>"}, -1, 0, ""),
		streamObj("BT /F1 12 Tf (chain end) Tj ET"),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	)
	return xrefStreamPDF(objs, map[int][2]int{1: {first, 0}})
}

func TestObjectStreamExtendsChainAtLimit(t *testing.T) {
	text, err := extract(t, extendsChainPDF(maxObjStmExtends))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "chain end") {
		t.Fatalf("text = %q, want the page content", text)
	}
}

func TestObjectStreamExtendsChainBeyondLimit(t *testing.T) {
	_, err := extract(t, extendsChainPDF(maxObjStmExtends+1))
	if err == nil || !strings.Contains(err.Error(), "Extends chain exceeds") {
		t.Fatalf("err = %v, want an Extends hop limit error", err)
	}
}

// encodePNGUp encodes data as PNG "Up" rows of the given width, the form the
// Predictor 12 reader expects.
func encodePNGUp(data []byte, columns int) []byte {
	var out bytes.Buffer
	prev := make([]byte, columns)
	for i := 0; i < len(data); i += columns {
		row := data[i : i+columns]
		out.WriteByte(2)
		for j, b := range row {
			out.WriteByte(b - prev[j])
		}
		prev = row
	}
	return out.Bytes()
}

func TestXrefStreamWithPredictor(t *testing.T) {
	objs := append([]string{}, pageObjs...)
	objs[4] = streamObj("BT /F1 12 Tf (predicted) Tj ET")
	data := xrefStreamPDF(objs, nil)

	// Re-encode the xref stream's table with FlateDecode and Predictor 12.
	start := bytes.Index(data, []byte("/Type /XRef"))
	head := bytes.LastIndex(data[:start], []byte("\n")) + 1
	raw := data[bytes.Index(data[start:], []byte("stream\n"))+start+len("stream\n"):]
	raw = raw[:bytes.Index(raw, []byte("\nendstream"))]

	var compressed bytes.Buffer
	w := zlib.NewWriter(&compressed)
	w.Write(encodePNGUp(raw, 9))
	w.Close()

	var pdf bytes.Buffer
	pdf.Write(data[:head])
	fmt.Fprintf(&pdf, "%d 0 obj\n<< /Type /XRef /Size %d /W [1 4 4] /Root 1 0 R /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns 9 >> /Length %d >>\nstream\n%s\nendstream\nendobj\n",
		len(objs), len(objs)+1, compressed.Len(), compressed.String())
	fmt.Fprintf(&pdf, "startxref\n%d\n%%%%EOF\n", head)

	text, err := extract(t, pdf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "predicted") {
		t.Fatalf("text = %q, want the page content", text)
	}
}

func TestContentStreamWithTwoFilters(t *testing.T) {
	content := []byte("BT /F1 12 Tf (two filters) Tj ET")

	var deflated bytes.Buffer
	w := zlib.NewWriter(&deflated)
	w.Write(content)
	w.Close()

	encoded := make([]byte, ascii85.MaxEncodedLen(deflated.Len()))
	encoded = encoded[:ascii85.Encode(encoded, deflated.Bytes())]

	objs := append([]string{}, pageObjs...)
	objs[4] = fmt.Sprintf("<< /Length %d /Filter [/ASCII85Decode /FlateDecode] >>\nstream\n%s\nendstream",
		len(encoded), encoded)

	text, err := extract(t, classicPDF(objs, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "two filters") {
		t.Fatalf("text = %q, want the page content", text)
	}
}
