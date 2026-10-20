package pdf

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// Tests for the opencode-go hardening changes. Each crafted file below
// used to hang, crash with an unrecoverable runtime error, or allocate
// without bound in the upstream code.

// pdfFile assembles a classic-xref PDF; objs[i] is the body of object i+1
// and object 1 is the catalog.
func pdfFile(objs ...string) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offs := make([]int, len(objs)+1)
	for i, body := range objs {
		offs[i+1] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&b, "%010d 00000 n \n", offs[i])
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

// streamObj renders a stream object body, Flate-compressing data when
// flate is set.
func streamObj(dict string, data []byte, flate bool) string {
	if flate {
		var z bytes.Buffer
		w := zlib.NewWriter(&z)
		w.Write(data)
		w.Close()
		data = z.Bytes()
		dict += " /Filter /FlateDecode"
	}
	return fmt.Sprintf("<< %s /Length %d >>\nstream\n%s\nendstream", dict, len(data), data)
}

// onePage builds a one-page document whose font F1 has the given
// dictionary body and whose page content is content.
func onePage(font string, content []byte, flate bool, extra ...string) []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		font,
		streamObj("", content, flate),
	}
	return pdfFile(append(objs, extra...)...)
}

const helvetica = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"

// xrefStreamPDF returns a file whose startxref points at an xref stream
// object with the given extra dictionary entries and raw data.
func xrefStreamPDF(dict string, data []byte) []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.5\n")
	off := b.Len()
	fmt.Fprintf(&b, "1 0 obj\n<< /Type /XRef %s /Length %d >>\nstream\n%s\nendstream\nendobj\n", dict, len(data), data)
	fmt.Fprintf(&b, "startxref\n%d\n%%%%EOF\n", off)
	return b.Bytes()
}

func openPDF(t *testing.T, data []byte) *Reader {
	t.Helper()
	r, err := NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r
}

func newReaderErr(data []byte) error {
	_, err := NewReader(bytes.NewReader(data), int64(len(data)))
	return err
}

// within runs fn on its own goroutine and returns what it panicked with
// (nil when it returned normally); the test fails if fn is still running
// after d — the upstream behavior these tests pin down is an endless loop.
func within(t *testing.T, d time.Duration, fn func()) any {
	t.Helper()
	done := make(chan any, 1)
	go func() {
		defer func() { done <- recover() }()
		fn()
	}()
	select {
	case p := <-done:
		return p
	case <-time.After(d):
		t.Fatalf("still running after %v", d)
		return nil
	}
}

func wantLimit(t *testing.T, v any, what string) {
	t.Helper()
	var le *LimitError
	switch x := v.(type) {
	case *LimitError:
		le = x
	case error:
		if !errors.As(x, &le) {
			t.Fatalf("got error %v, want a LimitError about %q", x, what)
		}
	default:
		t.Fatalf("got %v (%T), want a LimitError about %q", v, v, what)
	}
	if !strings.Contains(le.What, what) {
		t.Fatalf("LimitError %q, want one about %q", le.What, what)
	}
}

// drain is an Interpret callback that consumes every operand, like a
// real operator would.
func drain(stk *Stack, op string) {
	for stk.Len() > 0 {
		stk.Pop()
	}
}

func contents(r *Reader) Value {
	return r.Page(1).V.Key("Contents")
}

func TestHardeningPlainTextStillWorks(t *testing.T) {
	r := openPDF(t, onePage(helvetica, []byte("BT /F1 12 Tf 72 700 Td (Hello, PDF) Tj ET"), true))
	got, err := r.Page(1).GetPlainText(nil)
	if err != nil || !strings.Contains(got, "Hello, PDF") {
		t.Fatalf("GetPlainText = %q, %v", got, err)
	}
}

func TestHardeningHexStringAtEndOfStream(t *testing.T) {
	r := openPDF(t, onePage(helvetica, []byte("BT /F1 12 Tf <4142"), false))
	within(t, 3*time.Second, func() {
		Interpret(contents(r), drain)
	})
}

func TestHardeningXrefStreamSize(t *testing.T) {
	err := newReaderErr(xrefStreamPDF("/Size 2000000 /W [1 4 2]", nil))
	wantLimit(t, err, "xref stream Size")
}

func TestHardeningXrefTableObjectNumber(t *testing.T) {
	data := pdfFile("<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [] /Count 0 >>")
	// Add a subsection for object 5,000,000; the table used to grow to it.
	data = bytes.Replace(data, []byte("trailer"), []byte("5000000 1\n0000000009 00000 n \ntrailer"), 1)
	wantLimit(t, newReaderErr(data), "xref table")
}

func TestHardeningXrefStreamWidths(t *testing.T) {
	if err := newReaderErr(xrefStreamPDF("/Size 3 /W [1 100000 1]", nil)); err == nil ||
		!strings.Contains(err.Error(), "invalid W array") {
		t.Fatalf("huge /W width: got %v", err)
	}
	// readXrefStream wraps this error with %v, so only the text survives.
	if err := newReaderErr(xrefStreamPDF("/Size 3 /W [1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1 1]", nil)); err == nil ||
		!strings.Contains(err.Error(), "W array exceeds the safety limit") {
		t.Fatalf("17 /W fields: got %v", err)
	}
}

func TestHardeningPredictorColumns(t *testing.T) {
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	w.Write([]byte{2, 1, 0, 0})
	w.Close()
	err := newReaderErr(xrefStreamPDF(
		"/Size 1 /W [1 1 1] /Filter /FlateDecode /DecodeParms << /Predictor 12 /Columns 1000000 >>", z.Bytes()))
	wantLimit(t, err, "Columns")
}

func TestHardeningFilterChain(t *testing.T) {
	r := openPDF(t, onePage(helvetica, []byte("x"), false)) // empty streams skip filters
	strm := r.Page(1).V.Key("Contents")
	s := strm.data.(stream)
	s.hdr["Filter"] = array{name("FlateDecode"), name("FlateDecode"), name("FlateDecode"),
		name("FlateDecode"), name("FlateDecode"), name("FlateDecode"), name("FlateDecode"),
		name("FlateDecode"), name("FlateDecode")}
	wantLimit(t, within(t, 3*time.Second, func() { strm.Reader() }), "filter chain")
}

func TestHardeningInflateBudget(t *testing.T) {
	big := bytes.Repeat([]byte("0 0 m "), 1<<20) // 6 MiB, compresses to a few KiB
	r := openPDF(t, onePage(helvetica, big, true))
	r.SetInflateLimit(1 << 20)
	p := within(t, 5*time.Second, func() {
		Interpret(contents(r), drain)
	})
	wantLimit(t, p, "decompressed data")
}

func TestHardeningTokenTruncated(t *testing.T) {
	content := "BT (" + strings.Repeat("a", 3<<20) + ") Tj (after) Tj ET"
	r := openPDF(t, onePage(helvetica, []byte(content), true))
	var lens []int
	within(t, 10*time.Second, func() {
		Interpret(contents(r), func(stk *Stack, op string) {
			if op == "Tj" {
				lens = append(lens, len(stk.Pop().RawString()))
			}
		})
	})
	if len(lens) != 2 || lens[0] != maxTokenBytes || lens[1] != len("after") {
		t.Fatalf("Tj operand lengths = %v, want [%d 5]", lens, maxTokenBytes)
	}
}

func TestHardeningObjectElements(t *testing.T) {
	content := "[" + strings.Repeat("1 ", maxObjectElements+10) + "] Tj"
	r := openPDF(t, onePage(helvetica, []byte(content), true))
	p := within(t, 10*time.Second, func() {
		Interpret(contents(r), drain)
	})
	wantLimit(t, p, "object size")
}

func TestHardeningOperandStack(t *testing.T) {
	r := openPDF(t, onePage(helvetica, []byte(strings.Repeat("1 ", maxStackDepth+10)), true))
	p := within(t, 10*time.Second, func() {
		Interpret(contents(r), drain)
	})
	wantLimit(t, p, "operand stack")
}

func TestHardeningDictionaryLimits(t *testing.T) {
	r := openPDF(t, onePage(helvetica, []byte(strings.Repeat("<< >> begin ", 100)), false))
	wantLimit(t, within(t, 5*time.Second, func() {
		Interpret(contents(r), drain)
	}), "dictionary nesting")

	r = openPDF(t, onePage(helvetica, []byte("<< >> begin "+strings.Repeat("/k 1 def ", maxDictDefs+10)), true))
	wantLimit(t, within(t, 10*time.Second, func() {
		Interpret(contents(r), drain)
	}), "dictionary definitions")
}

// cmapFont returns a Type0-style font dictionary whose ToUnicode is
// object 6 (pass the stream through onePage's extra objects).
const cmapFont = "<< /Type /Font /Subtype /Type0 /BaseFont /X /Encoding /Identity-H /ToUnicode 6 0 R >>"

func cmapOf(t *testing.T, cmapSrc string) *cmap {
	t.Helper()
	r := openPDF(t, onePage(cmapFont, nil, false, streamObj("", []byte(cmapSrc), true)))
	var m *cmap
	within(t, 30*time.Second, func() {
		m = readCmap(r.Page(1).Font("F1").V.Key("ToUnicode"))
	})
	if m == nil {
		t.Fatal("readCmap returned nil")
	}
	return m
}

func TestHardeningCmapCountBoundedByStack(t *testing.T) {
	m := cmapOf(t, "1 begincodespacerange <00> <FF> endcodespacerange\n"+
		"3000000 beginbfchar <41> <0042> endbfchar\n"+
		"2000000 beginbfrange <61> <62> <0063> endbfrange\n")
	if len(m.bfchar) != 1 || len(m.bfrange) != 1 {
		t.Fatalf("bfchar=%d bfrange=%d, want 1 and 1 (counts bounded by the operands present)", len(m.bfchar), len(m.bfrange))
	}
	if got := m.Decode("Ab"); got != "Bd" {
		t.Fatalf("Decode = %q, want %q", got, "Bd")
	}
}

func TestHardeningCmapEntryBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString("1 begincodespacerange <00> <FF> endcodespacerange\n")
	block := "30000 beginbfchar\n" + strings.Repeat("<41> <0042>\n", 30000) + "endbfchar\n"
	for i := 0; i < 9; i++ {
		b.WriteString(block)
	}
	m := cmapOf(t, b.String())
	if want := maxCmapEntries - 1; len(m.bfchar) != want {
		t.Fatalf("bfchar entries = %d, want %d (budget minus the codespace range)", len(m.bfchar), want)
	}
}

func TestHardeningCodespaceLongCodes(t *testing.T) {
	r := openPDF(t, onePage(cmapFont, nil, false,
		streamObj("", []byte("1 begincodespacerange <0000000000> <FFFFFFFFFF> endcodespacerange"), false)))
	var m *cmap
	if p := within(t, 3*time.Second, func() {
		m = readCmap(r.Page(1).Font("F1").V.Key("ToUnicode"))
	}); p != nil {
		t.Fatalf("readCmap panicked: %v", p)
	}
	if m != nil {
		t.Fatal("5-byte codespace range accepted")
	}
}

func TestHardeningEmptyRangeDestination(t *testing.T) {
	m := cmapOf(t, "1 begincodespacerange <00> <FF> endcodespacerange\n"+
		"1 beginbfrange <41> <43> <> endbfrange\n")
	if p := within(t, 3*time.Second, func() { m.Decode("B") }); p != nil {
		t.Fatalf("Decode panicked: %v", p)
	}
}

func TestHardeningUTF16OddLength(t *testing.T) {
	var got string
	if p := within(t, time.Second, func() { got = utf16Decode("\x00A\x00") }); p != nil {
		t.Fatalf("utf16Decode panicked: %v", p)
	}
	if got != "A" {
		t.Fatalf("utf16Decode = %q, want %q", got, "A")
	}
}

func TestHardeningPageTreeCycle(t *testing.T) {
	r := openPDF(t, pdfFile("<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [2 0 R] /Count 5 >>"))
	var p Page
	within(t, 10*time.Second, func() { p = r.Page(1) })
	if !p.V.IsNull() {
		t.Fatalf("cyclic page tree produced a page: %v", p.V)
	}
}

func TestHardeningInheritCycle(t *testing.T) {
	r := openPDF(t, pdfFile(
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 3 0 R >>",
	))
	var res Value
	within(t, 5*time.Second, func() { res = r.Page(1).Resources() })
	if !res.IsNull() {
		t.Fatalf("Resources = %v, want null", res)
	}
}

func TestHardeningObjectStreamHeaderAtEOF(t *testing.T) {
	r := openPDF(t, pdfFile(streamObj("/Type /ObjStm /N 1000000000 /First 4", []byte("1 0 "), false)))
	// Point object 7 into the object stream; its header ends long before
	// the declared count.
	for len(r.xref) <= 7 {
		r.xref = append(r.xref, xref{})
	}
	r.xref[7] = xref{ptr: objptr{7, 0}, inStream: true, stream: objptr{1, 0}}
	p := within(t, 5*time.Second, func() { r.resolve(objptr{}, objptr{7, 0}) })
	if p == nil || !strings.Contains(fmt.Sprint(p), "cannot find object") {
		t.Fatalf("resolve = %v, want the missing-object panic", p)
	}
}

func TestHardeningDifferencesTable(t *testing.T) {
	font := "<< /Type /Font /Subtype /Type1 /BaseFont /X /Encoding << /Differences [65 /B /C 65 /D 97 /alpha 98 /notaglyph 98 /E] >> >>"
	r := openPDF(t, onePage(font, nil, false))
	f := r.Page(1).Font("F1")
	if got := f.Encoder().Decode("ABab z"); got != "BCαE z" {
		t.Fatalf("Decode = %q, want %q (first known name per code wins)", got, "BCαE z")
	}

	// A huge Differences array used to be rescanned for every byte; with
	// only unknown glyph names no scan ever stopped early.
	big := "<< /Type /Font /Subtype /Type1 /BaseFont /X /Encoding << /Differences [0 " +
		strings.Repeat("/notaglyph ", 200000) + "/a] >> >>"
	r = openPDF(t, onePage(big, nil, false))
	f = r.Page(1).Font("F1")
	var got string
	within(t, 10*time.Second, func() { got = f.Encoder().Decode(strings.Repeat("x", 100000)) })
	if got != strings.Repeat("x", 100000) {
		t.Fatalf("Decode of big Differences = %.20q...", got)
	}
}

// decodeReference is the upstream cmap.Decode (linear scans), kept to
// check that the indexed lookups return identical text.
func decodeReference(m *cmap, raw string) string {
	var r []rune
Parse:
	for len(raw) > 0 {
		for n := 1; n <= 4 && n <= len(raw); n++ {
			for _, space := range m.space[n-1] {
				if space.low <= raw[:n] && raw[:n] <= space.high {
					text := raw[:n]
					raw = raw[n:]
					for _, bfchar := range m.bfchar {
						if len(bfchar.orig) == n && bfchar.orig == text {
							r = append(r, []rune(utf16Decode(bfchar.repl))...)
							continue Parse
						}
					}
					for _, bfrange := range m.bfrange {
						if len(bfrange.lo) == n && bfrange.lo <= text && text <= bfrange.hi {
							if bfrange.dst.Kind() == String {
								s := bfrange.dst.RawString()
								if bfrange.lo != text && s != "" {
									b := []byte(s)
									b[len(b)-1] += text[len(text)-1] - bfrange.lo[len(bfrange.lo)-1]
									s = string(b)
								}
								r = append(r, []rune(utf16Decode(s))...)
								continue Parse
							}
							r = append(r, noRune)
							continue Parse
						}
					}
					r = append(r, noRune)
					continue Parse
				}
			}
		}
		r = append(r, noRune)
		raw = raw[1:]
	}
	return string(r)
}

func TestHardeningCmapIndexMatchesLinearScan(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	code := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.Intn(8)) // small alphabet: plenty of collisions
		}
		return string(b)
	}
	for trial := 0; trial < 300; trial++ {
		m := &cmap{}
		m.space[0] = []byteRange{{"\x00", "\x03"}}
		m.space[1] = []byteRange{{"\x04\x00", "\x07\xff"}}
		for i := rng.Intn(12); i > 0; i-- {
			n := 1 + rng.Intn(2)
			m.bfchar = append(m.bfchar, bfchar{code(n), "\x00" + string(rune('a'+rng.Intn(26)))})
		}
		overlap := trial%2 == 0
		lo := byte(0)
		for i := rng.Intn(12); i > 0; i-- {
			n := 1 + rng.Intn(2)
			a := code(n)
			if !overlap { // ascending, disjoint ranges
				a = strings.Repeat("\x00", n-1) + string([]byte{lo})
				lo += 3
			}
			hi := []byte(a)
			hi[len(hi)-1] += byte(rng.Intn(3))
			m.bfrange = append(m.bfrange, bfrange{a, string(hi), Value{data: "\x00" + string(rune('A'+rng.Intn(26)))}})
		}
		for k := 0; k < 20; k++ {
			raw := code(1 + rng.Intn(12))
			if got, want := m.Decode(raw), decodeReference(m, raw); got != want {
				t.Fatalf("trial %d: Decode(%q) = %q, linear scan = %q (cmap %+v)", trial, raw, got, want, m)
			}
		}
	}
}

func TestHardeningCmapDecodeScales(t *testing.T) {
	var b strings.Builder
	b.WriteString("1 begincodespacerange <0000> <FFFF> endcodespacerange\n")
	for blk := 0; blk < 5; blk++ {
		b.WriteString("10000 beginbfchar\n")
		for i := 0; i < 10000; i++ {
			fmt.Fprintf(&b, "<%04X> <%04X>\n", blk*10000+i, 0x4E00+(blk*10000+i)%0x5000)
		}
		b.WriteString("endbfchar\n")
	}
	m := cmapOf(t, b.String())
	// The last code declared: a linear scan walks all 50,000 entries for
	// every character.
	last := 5*10000 - 1
	raw := strings.Repeat(string([]byte{byte(last >> 8), byte(last)}), 200000)
	var got string
	within(t, 3*time.Second, func() { got = m.Decode(raw) })
	if want := strings.Repeat(string(rune(0x4E00+last%0x5000)), 200000); got != want {
		t.Fatalf("Decode = %.12q..., want %.12q...", got, want)
	}
}

func TestHardeningCmapWorkBudget(t *testing.T) {
	// Overlapping ranges disable the binary search; a long string then
	// costs len(bfrange) comparisons per code, which the budget bounds.
	var b strings.Builder
	b.WriteString("1 begincodespacerange <00> <FF> endcodespacerange\n10000 beginbfrange\n")
	for i := 0; i < 10000; i++ {
		b.WriteString("<00> <FE> <0041>\n")
	}
	b.WriteString("endbfrange\n")
	m := cmapOf(t, b.String())
	p := within(t, 30*time.Second, func() {
		for i := 0; i < 100; i++ {
			m.Decode(strings.Repeat("\xff", 1<<20))
		}
	})
	wantLimit(t, p, "decoding work")
}
