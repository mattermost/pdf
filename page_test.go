package pdf

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// cmapPDF builds a one-page PDF whose /F1 font carries the given ToUnicode
// character map and whose content stream is content.
func cmapPDF(cmap, content string) []byte {
	return buildPagePDF("/Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >>",
		streamObj(content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Mapped /ToUnicode 6 0 R >>",
		streamObj(cmap),
	)
}

// cmapSource wraps the body of a character map in the usual preamble.
func cmapSource(body string) string {
	return strings.Join([]string{
		"/CIDInit /ProcSet findresource begin",
		"12 dict begin",
		"begincmap",
		"/CMapName /Test def",
		"/CMapType 2 def",
		"1 begincodespacerange",
		"<00> <FF>",
		"endcodespacerange",
		body,
		"endcmap",
		"CMapName currentdict /CMap defineresource pop",
		"end",
		"end",
	}, "\n")
}

func TestPageTreeKidsRepeatedNode(t *testing.T) {
	data := classicPDF([]string{
		"",
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Pages /Parent 2 0 R /Kids [2 0 R] /Count 1 >>",
	}, "")

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Fatalf("text = %q, want no text", text)
	}
}

func TestPageTreeKidsSelfReference(t *testing.T) {
	data := classicPDF([]string{
		"",
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [2 0 R] /Count 1 >>",
	}, "")

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if text != "" {
		t.Fatalf("text = %q, want no text", text)
	}
}

func TestPageTreeNestedResolves(t *testing.T) {
	data := classicPDF([]string{
		"",
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 2 >>",
		"<< /Type /Pages /Parent 2 0 R /Kids [4 0 R 5 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 3 0 R /MediaBox [0 0 200 200] /Contents 6 0 R /Resources << /Font << /F1 8 0 R >> >> >>",
		"<< /Type /Page /Parent 3 0 R /MediaBox [0 0 200 200] /Contents 7 0 R /Resources << /Font << /F1 8 0 R >> >> >>",
		streamObj("BT /F1 12 Tf (one) Tj ET"),
		streamObj("BT /F1 12 Tf (two) Tj ET"),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}, "")

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "one") || !strings.Contains(text, "two") {
		t.Fatalf("text = %q, want both pages", text)
	}
}

func TestResourcesInheritedFromAncestor(t *testing.T) {
	data := classicPDF([]string{
		"",
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 /Resources << /Font << /F1 5 0 R >> >> >>",
		"<< /Type /Pages /Parent 2 0 R /Kids [4 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 3 0 R /MediaBox [0 0 200 200] /Contents 6 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		streamObj("BT /F1 12 Tf (inherited) Tj ET"),
	}, "")

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "inherited") {
		t.Fatalf("text = %q, want the page content", text)
	}

	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Page(1).Resources().Key("Font").Keys(); len(got) != 1 || got[0] != "F1" {
		t.Fatalf("fonts = %v, want [F1]", got)
	}
}

func TestResourcesParentChainRepeatedNode(t *testing.T) {
	data := classicPDF([]string{
		"",
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 4 0 R /MediaBox [0 0 200 200] >>",
		"<< /Type /Pages /Parent 3 0 R /Kids [3 0 R] /Count 1 >>",
	}, "")

	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	withinTimeout(t, func() {
		if res := r.Page(1).Resources(); !res.IsNull() {
			t.Errorf("resources = %v, want null", res)
		}
	})
}

// parentChainPDF builds a one-page PDF whose page sits under depth ancestor
// nodes, with the resources dictionary on the topmost one.
func parentChainPDF(depth int) []byte {
	objs := []string{
		"",
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		fmt.Sprintf("<< /Type /Page /Parent 4 0 R /MediaBox [0 0 200 200] /Contents %d 0 R >>", depth+4),
	}
	for i := 0; i < depth; i++ {
		if i == depth-1 {
			objs = append(objs, "<< /Type /Pages /Count 1 /Resources << /Font << /F1 "+fmt.Sprint(depth+5)+" 0 R >> >> >>")
			break
		}
		objs = append(objs, fmt.Sprintf("<< /Type /Pages /Parent %d 0 R /Count 1 >>", i+5))
	}
	objs = append(objs,
		streamObj("BT /F1 12 Tf (deep) Tj ET"),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	)
	return classicPDF(objs, "")
}

func TestResourcesShortParentChain(t *testing.T) {
	text, err := extract(t, parentChainPDF(10))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "deep") {
		t.Fatalf("text = %q, want the page content", text)
	}
}

func TestResourcesParentChainBeyondLimit(t *testing.T) {
	data := parentChainPDF(maxPageTreeDepth + 10)
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	withinTimeout(t, func() {
		if res := r.Page(1).Resources(); !res.IsNull() {
			t.Errorf("resources = %v, want null", res)
		}
	})
}

func TestCmapRealistic(t *testing.T) {
	body := strings.Join([]string{
		"2 beginbfchar",
		"<41> <0058>",
		"<42> <0059>",
		"endbfchar",
		"1 beginbfrange",
		"<61> <63> <0030>",
		"endbfrange",
	}, "\n")
	text, err := extract(t, cmapPDF(cmapSource(body), "BT /F1 12 Tf <41426162> Tj ET"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(text); got != "XY01" {
		t.Fatalf("text = %q, want %q", got, "XY01")
	}
}

func TestCmapBfcharCountBeyondOperands(t *testing.T) {
	body := strings.Join([]string{
		"1000000 beginbfchar",
		"<41> <0058>",
		"endbfchar",
	}, "\n")
	text, err := extract(t, cmapPDF(cmapSource(body), "BT /F1 12 Tf <41> Tj ET"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(text); got != "X" {
		t.Fatalf("text = %q, want %q", got, "X")
	}
}

func TestCmapBfrangeCountBeyondOperands(t *testing.T) {
	body := strings.Join([]string{
		"1000000 beginbfrange",
		"<61> <63> <0030>",
		"endbfrange",
	}, "\n")
	text, err := extract(t, cmapPDF(cmapSource(body), "BT /F1 12 Tf <616263> Tj ET"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(text); got != "012" {
		t.Fatalf("text = %q, want %q", got, "012")
	}
}

func TestCmapCountsClampToOperands(t *testing.T) {
	body := strings.Join([]string{
		"100000 beginbfchar",
		"<41> <0058>",
		"endbfchar",
		"100000 beginbfrange",
		"<61> <63> <0030>",
		"endbfrange",
	}, "\n")
	data := cmapPDF(cmapSource(body), "BT /F1 12 Tf <41> Tj ET")
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	m := readCmap(context.Background(), r.Page(1).Font("F1").V.Key("ToUnicode"))
	if m == nil {
		t.Fatal("no character map")
	}
	if len(m.bfchar) != 1 || len(m.bfrange) != 1 {
		t.Fatalf("got %d bfchar and %d bfrange entries, want 1 and 1", len(m.bfchar), len(m.bfrange))
	}
}

func TestCmapRepeatedEndOperator(t *testing.T) {
	// The second endbfchar has no count of its own to work from.
	body := strings.Join([]string{
		"1 beginbfchar",
		"<41> <0058>",
		"endbfchar",
		"endbfchar",
	}, "\n")
	text, err := extract(t, cmapPDF(cmapSource(body), "BT /F1 12 Tf <41> Tj ET"))
	if got := strings.TrimSpace(text); err == nil && got != "X" {
		t.Fatalf("text = %q, want %q or an error", got, "X")
	}
}

func TestCmapDecodeStopsWhenContextDone(t *testing.T) {
	m := &cmap{}
	m.space[0] = []byteRange{{"\x00", "\xff"}}
	m.bfchar = []bfchar{{"A", "\x00X"}}

	if got := m.Decode("A"); got != "X" {
		t.Fatalf("Decode = %q, want %q", got, "X")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := m.decodeContext(ctx, "A"); got != "" {
		t.Fatalf("decodeContext = %q, want no text", got)
	}
}

func TestDifferencesTable(t *testing.T) {
	arr := func(items ...object) Value { return Value{nil, objptr{}, array(items)} }
	tests := []struct {
		name        string
		differences Value
		code        byte
		want        rune
	}{
		{"empty", arr(), 'A', 'A'},
		{"named_code", arr(int64(65), name("adieresis")), 65, 'ä'},
		{"code_advances", arr(int64(65), name("adieresis"), name("ccedilla")), 66, 'ç'},
		{"untouched_code", arr(int64(65), name("adieresis")), 64, '@'},
		{"first_name_wins", arr(int64(65), name("adieresis"), int64(65), name("ccedilla")), 65, 'ä'},
		{"unknown_name_keeps_code", arr(int64(65), name("notaglyph"), name("ccedilla")), 65, 'A'},
		{"unknown_name_advances", arr(int64(65), name("notaglyph"), name("ccedilla")), 66, 'ç'},
		{"code_out_of_range", arr(int64(300), name("adieresis")), 44, ','},
		{"negative_code", arr(int64(-5), name("adieresis"), name("ccedilla")), 0, 0},
		{"other_types_ignored", arr(int64(65), "text", name("bullet")), 65, '•'},
		{"real_is_not_a_code", arr(float64(65.5), name("adieresis")), 65, 'A'},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := differencesTable(tc.differences)[tc.code]; got != tc.want {
				t.Fatalf("table[%d] = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

func TestDifferencesEncodingDecodesText(t *testing.T) {
	data := buildPagePDF("/Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >>",
		streamObj("BT /F1 12 Tf (AB) Tj ET"),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Mapped /Encoding << /Type /Encoding /Differences [65 /adieresis /ccedilla] >> >>",
	)

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(text); got != "äç" {
		t.Fatalf("text = %q, want %q", got, "äç")
	}
}

func TestPageWithSeveralFonts(t *testing.T) {
	font := func(first string) string {
		return "<< /Type /Font /Subtype /Type1 /BaseFont /Mapped /Encoding << /Differences [65 /" + first + "] >> >>"
	}
	data := buildPagePDF("/Contents 4 0 R /Resources << /Font << /F1 5 0 R /F2 6 0 R /F3 7 0 R >> >>",
		streamObj("BT /F1 12 Tf (A) Tj /F2 12 Tf (A) Tj /F3 12 Tf (A) Tj ET"),
		font("adieresis"),
		font("ccedilla"),
		font("bullet"),
	)

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(text); got != "äç•" {
		t.Fatalf("text = %q, want %q", got, "äç•")
	}
}

func TestInlineFontsInIndirectResources(t *testing.T) {
	font := func(first string) string {
		return "<< /Type /Font /Subtype /Type1 /BaseFont /Mapped /Encoding << /Differences [65 /" + first + "] >> >>"
	}
	data := buildPagePDF("/Contents 4 0 R /Resources 5 0 R",
		streamObj("BT /F1 12 Tf (A) Tj /F2 12 Tf (A) Tj ET"),
		"<< /Font << /F1 "+font("adieresis")+" /F2 "+font("ccedilla")+" >> >>",
	)

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(text); got != "äç" {
		t.Fatalf("text = %q, want %q", got, "äç")
	}
}

func TestFontNamesAreScopedToPage(t *testing.T) {
	font := func(first string) string {
		return "<< /Type /Font /Subtype /Type1 /BaseFont /Mapped /Encoding << /Differences [65 /" + first + "] >> >>"
	}
	page := func(content, font int) string {
		return fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", content, font)
	}
	data := classicPDF([]string{
		"",
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
		page(5, 7),
		page(6, 8),
		streamObj("BT /F1 12 Tf (A) Tj ET"),
		streamObj("BT /F1 12 Tf (A) Tj ET"),
		font("adieresis"),
		font("ccedilla"),
	}, "")

	text, err := extract(t, data)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(strings.Fields(text), ""); got != "äç" {
		t.Fatalf("text = %q, want %q", got, "äç")
	}
}

// manyFontsPDF builds a one-page PDF naming n fonts, each used once.
func manyFontsPDF(n int) []byte {
	var names, content bytes.Buffer
	content.WriteString("BT ")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&names, "/F%d %d 0 R ", i, i+7)
		fmt.Fprintf(&content, "/F%d 12 Tf (A) Tj ", i)
	}
	content.WriteString("ET")

	objs := []string{
		streamObj(content.String()),
		"<< /Font 6 0 R >>",
		"<< " + names.String() + ">>",
	}
	for i := 0; i < n; i++ {
		objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	}
	return buildPagePDF("/Contents 4 0 R /Resources 5 0 R", objs...)
}

// errAfterCtx reports cancellation once Err has been called n times.
type errAfterCtx struct {
	context.Context
	n int
}

func (c *errAfterCtx) Err() error {
	if c.n <= 0 {
		return context.Canceled
	}
	c.n--
	return nil
}

func TestFontLoopHonoursContext(t *testing.T) {
	data := manyFontsPDF(4)
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	// The first Err call is the one GetPlainText makes before any page work.
	ctx := &errAfterCtx{Context: context.Background(), n: 1}
	if _, err := r.GetPlainText(ctx); err != context.Canceled {
		t.Fatalf("err = %v, want %v", err, context.Canceled)
	}
}

// countingReaderAt counts how many times a Reader starts reading at watch.
type countingReaderAt struct {
	r     *bytes.Reader
	watch int64
	hits  int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off == c.watch {
		c.hits++
	}
	return c.r.ReadAt(p, off)
}

func TestFontDictionaryReadOncePerPage(t *testing.T) {
	data := manyFontsPDF(20)
	// Object 6 is the page's font dictionary.
	f := &countingReaderAt{r: bytes.NewReader(data), watch: int64(bytes.Index(data, []byte("6 0 obj")))}
	r, err := NewReader(f, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetPlainText(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.hits != 1 {
		t.Fatalf("font dictionary read %d times, want 1", f.hits)
	}
}
