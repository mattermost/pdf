// Generate adversarial PDFs that stress cancellation / allocation gaps in mattermost/pdf.
//
// Usage:
//
//	go run ./tools/gen_adversarial_pdfs -o tools/adversarial_pdfs --scale medium
//	go run ./tools/gen_adversarial_pdfs -o /tmp/pdfs --scale large many_operators nested_content
package main

import (
	"bytes"
	"compress/zlib"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// --- PDF writing helpers ---

func xrefAndTrailer(body []byte, offsets []int, rootObj int) []byte {
	xrefPos := len(body)
	n := len(offsets)
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "xref\n0 %d\n", n)
	buf.WriteString("0000000000 65535 f \r\n")
	for _, off := range offsets[1:] {
		fmt.Fprintf(&buf, "%010d 00000 n \r\n", off)
	}
	fmt.Fprintf(&buf, "trailer << /Size %d /Root %d 0 R >>\nstartxref\n%d\n", n, rootObj, xrefPos)
	buf.WriteString("%%EOF\n")
	return buf.Bytes()
}

func writeObjects(objs [][]byte) []byte {
	var body []byte
	body = append(body, "%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"...)
	offsets := make([]int, len(objs))
	for i := 1; i < len(objs); i++ {
		offsets[i] = len(body)
		body = append(body, fmt.Sprintf("%d 0 obj\n", i)...)
		body = append(body, objs[i]...)
		body = append(body, "\nendobj\n"...)
	}
	return append(body, xrefAndTrailer(body, offsets, 1)...)
}

func pdfStream(data []byte, extraDict string) []byte {
	header := fmt.Sprintf("<< /Length %d%s >>\nstream\n", len(data), extraDict)
	result := make([]byte, 0, len(header)+len(data)+len("\nendstream"))
	result = append(result, header...)
	result = append(result, data...)
	return append(result, "\nendstream"...)
}

func flateStream(raw []byte, extraDict string) []byte {
	var buf bytes.Buffer
	w, _ := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	_, _ = w.Write(raw)
	_ = w.Close()
	return pdfStream(buf.Bytes(), " /Filter /FlateDecode"+extraDict)
}

func simplePageWithContent(content []byte, useFlate bool) []byte {
	var contentObj []byte
	if useFlate {
		contentObj = flateStream(content, "")
	} else {
		contentObj = pdfStream(content, "")
	}
	return writeObjects([][]byte{
		{},
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>"),
		contentObj,
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	})
}

// --- Fixture builders ---

// buildToUnicodeCmap builds a page whose font has a large ToUnicode CMap.
// GetPlainText → Tf → Font.Encoder() → readCmap → Interpret(context.Background()),
// so cancellation of the outer ctx does not stop CMap parsing.
func buildToUnicodeCmap(bfcharCount int) []byte {
	var lines []string
	lines = append(lines,
		"/CIDInit /ProcSet findresource begin",
		"12 dict begin",
		"begincmap",
		"/CIDSystemInfo << /Registry (Adobe) /Ordering (UCS) /Supplement 0 >> def",
		"/CMapName /Adobe-Identity-UCS def",
		"/CMapType 2 def",
		"1 begincodespacerange",
		"<0000> <FFFF>",
		"endcodespacerange",
	)
	remaining, code := bfcharCount, 0
	for remaining > 0 {
		n := min(100, remaining)
		lines = append(lines, fmt.Sprintf("%d beginbfchar", n))
		for range n {
			lines = append(lines, fmt.Sprintf("<%04X> <%02X>", code, code&0xFF))
			code = (code + 1) & 0xFFFF
		}
		lines = append(lines, "endbfchar")
		remaining -= n
	}
	lines = append(lines, "endcmap", "CMapName currentdict /CMap defineresource pop", "end", "end")

	return writeObjects([][]byte{
		{},
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>"),
		pdfStream([]byte("BT /F1 12 Tf 10 700 Td (x) Tj ET"), ""),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Evil /ToUnicode 6 0 R >>"),
		flateStream([]byte(strings.Join(lines, "\n")), ""),
	})
}

// buildHugeLiteralTj builds a page with a single Tj whose literal string is stringBytes long.
// Interpret checks ctx only before readToken; readLiteralString appends the entire payload
// with no further cancel checks.
func buildHugeLiteralTj(stringBytes int) []byte {
	content := make([]byte, 0, len("BT /F1 12 Tf 10 700 Td (")+stringBytes+len(") Tj ET"))
	content = append(content, "BT /F1 12 Tf 10 700 Td ("...)
	content = append(content, bytes.Repeat([]byte("A"), stringBytes)...)
	content = append(content, ") Tj ET"...)
	return simplePageWithContent(content, false)
}

// buildFlateBombTj builds a small on-disk PDF whose FlateDecode content stream
// expands into one huge literal Tj string.
func buildFlateBombTj(expandedBytes int) []byte {
	content := make([]byte, 0, len("BT /F1 12 Tf 10 700 Td (")+expandedBytes+len(") Tj ET"))
	content = append(content, "BT /F1 12 Tf 10 700 Td ("...)
	content = append(content, bytes.Repeat([]byte("A"), expandedBytes)...)
	content = append(content, ") Tj ET"...)
	return simplePageWithContent(content, true)
}

// buildPredictorColumns builds a page whose content stream uses Predictor 12
// with a huge Columns value. applyFilter allocates hist/tmp of length 1+columns
// before any Interpret loop.
func buildPredictorColumns(columns int) []byte {
	var compressed bytes.Buffer
	w, _ := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	_, _ = w.Write([]byte("q Q\n"))
	_ = w.Close()
	extras := fmt.Sprintf(" /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns %d >>", columns)
	return writeObjects([][]byte{
		{},
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>"),
		pdfStream(compressed.Bytes(), extras),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	})
}

// buildManyOperators builds a control case: many tiny Tj operators.
// Cancel should stop between them.
func buildManyOperators(opCount int) []byte {
	content := make([]byte, 0, len("BT /F1 12 Tf 10 700 Td")+opCount*len(" (.) Tj")+len(" ET"))
	content = append(content, "BT /F1 12 Tf 10 700 Td"...)
	for range opCount {
		content = append(content, " (.) Tj"...)
	}
	content = append(content, " ET"...)
	return simplePageWithContent(content, false)
}

// buildAcroformFields builds a page with many AcroForm fields.
// Negative control: GetPlainText never walks /AcroForm.
func buildAcroformFields(fieldCount int) []byte {
	objs := make([][]byte, 7+fieldCount)
	objs[0] = []byte{}

	refs := make([]string, fieldCount)
	for i := range fieldCount {
		refs[i] = fmt.Sprintf("%d 0 R", 7+i)
	}
	objs[1] = []byte("<< /Type /Catalog /Pages 2 0 R /AcroForm 6 0 R >>")
	objs[2] = []byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	objs[3] = []byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	objs[4] = pdfStream([]byte("BT /F1 12 Tf 10 700 Td (hello) Tj ET"), "")
	objs[5] = []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	objs[6] = []byte(fmt.Sprintf("<< /Fields [%s] /NeedAppearances true >>", strings.Join(refs, " ")))
	xs := strings.Repeat("X", 64)
	for i := range fieldCount {
		objs[7+i] = []byte(fmt.Sprintf(
			"<< /FT /Tx /T (field%d) /V (%s) /Rect [0 0 10 10] /Subtype /Widget /Type /Annot >>",
			i, xs,
		))
	}
	return writeObjects(objs)
}

// buildNestedContent builds a page whose first content-stream object is deeply nested.
// Hits maxObjectDepth (1000) when depth is large enough.
func buildNestedContent(depth int) []byte {
	content := make([]byte, 0, depth+1+depth+len(" BT /F1 12 Tf 10 700 Td (x) Tj ET"))
	content = append(content, bytes.Repeat([]byte("["), depth)...)
	content = append(content, '1')
	content = append(content, bytes.Repeat([]byte("]"), depth)...)
	content = append(content, " BT /F1 12 Tf 10 700 Td (x) Tj ET"...)
	return simplePageWithContent(content, false)
}

// --- Scales ---

type scaleConfig struct {
	toUnicodeCmap  int
	hugeLiteralTj  int
	flateBombTj    int
	predictorCols  int
	manyOperators  int
	acroformFields int
	nestedContent  int
}

var scales = map[string]scaleConfig{
	"small": {
		toUnicodeCmap:  2_000,
		hugeLiteralTj:  1_000_000,
		flateBombTj:    5_000_000,
		predictorCols:  5_000_000,
		manyOperators:  10_000,
		acroformFields: 1_000,
		nestedContent:  1_200, // must exceed maxObjectDepth=1000
	},
	"medium": {
		toUnicodeCmap:  50_000,
		hugeLiteralTj:  16_000_000,
		flateBombTj:    32_000_000,
		predictorCols:  16_000_000,
		manyOperators:  80_000,
		acroformFields: 10_000,
		nestedContent:  1_200,
	},
	"large": {
		toUnicodeCmap:  200_000,
		hugeLiteralTj:  64_000_000,
		flateBombTj:    256_000_000,
		predictorCols:  64_000_000,
		manyOperators:  200_000,
		acroformFields: 50_000,
		nestedContent:  1_200,
	},
}

type builder struct {
	name  string
	build func(scaleConfig) []byte
}

var builders = []builder{
	{"tounicode_cmap", func(c scaleConfig) []byte { return buildToUnicodeCmap(c.toUnicodeCmap) }},
	{"huge_literal_tj", func(c scaleConfig) []byte { return buildHugeLiteralTj(c.hugeLiteralTj) }},
	{"flate_bomb_tj", func(c scaleConfig) []byte { return buildFlateBombTj(c.flateBombTj) }},
	{"predictor_columns", func(c scaleConfig) []byte { return buildPredictorColumns(c.predictorCols) }},
	{"many_operators", func(c scaleConfig) []byte { return buildManyOperators(c.manyOperators) }},
	{"acroform_fields", func(c scaleConfig) []byte { return buildAcroformFields(c.acroformFields) }},
	{"nested_content", func(c scaleConfig) []byte { return buildNestedContent(c.nestedContent) }},
}

func main() {
	outdir := flag.String("o", "tools/adversarial_pdfs", "output directory")
	scale := flag.String("scale", "medium", "fixture scale: small, medium, large")
	flag.Parse()

	cfg, ok := scales[*scale]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown scale %q (want: small, medium, large)\n", *scale)
		os.Exit(2)
	}

	if err := os.MkdirAll(*outdir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	selected := flag.Args()

	if len(selected) > 0 {
		known := make(map[string]bool, len(builders))
		for _, b := range builders {
			known[b.name] = true
		}
		for _, name := range selected {
			if !known[name] {
				fmt.Fprintf(os.Stderr, "unknown fixture %q\n", name)
				os.Exit(2)
			}
		}
	}

	want := make(map[string]bool, len(selected))
	for _, name := range selected {
		want[name] = true
	}

	for _, b := range builders {
		if len(want) > 0 && !want[b.name] {
			continue
		}
		data := b.build(cfg)
		path := filepath.Join(*outdir, b.name+"."+*scale+".pdf")
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s (%d bytes on disk)\n", path, len(data))
	}
}
