package pdftext

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---- minimal PDF builder ----

type pdfBuilder struct {
	objs    []string
	trailer string // extra trailer entries
}

// add appends an object body and returns its object number.
func (b *pdfBuilder) add(body string) int {
	b.objs = append(b.objs, body)
	return len(b.objs)
}

// set replaces the body of object n (for forward references).
func (b *pdfBuilder) set(n int, body string) { b.objs[n-1] = body }

func streamBody(dict string, data []byte, flate bool) string {
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

func (b *pdfBuilder) bytes() []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offs := make([]int, len(b.objs)+1)
	for i, body := range b.objs {
		offs[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(b.objs)+1)
	for i := 1; i <= len(b.objs); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offs[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R %s >>\nstartxref\n%d\n%%%%EOF\n", len(b.objs)+1, b.trailer, xref)
	return out.Bytes()
}

const helvetica = "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>"

// doc builds a document with one page per content string; every page uses
// font F1 (fontBody) and the optional extra resources.
func doc(fontBody string, extraRes string, contents ...string) []byte {
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("") // pages, filled below
	font := b.add(fontBody)
	var kids []string
	for _, c := range contents {
		cs := b.add(streamBody("", []byte(c), true))
		p := b.add(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
			"/Resources << /Font << /F1 %d 0 R >> %s >> /Contents %d 0 R >>", font, extraRes, cs))
		kids = append(kids, fmt.Sprintf("%d 0 R", p))
	}
	b.set(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(kids)))
	return b.bytes()
}

// fresh extracts without the cache, so each call parses.
func fresh(t *testing.T, data []byte) Result {
	t.Helper()
	done := make(chan Result, 1)
	go func() { done <- extract(data, time.Now().Add(timeLimit)) }()
	select {
	case r := <-done:
		return r
	case <-time.After(30 * time.Second):
		t.Fatal("extraction did not finish")
		return Result{}
	}
}

func wantText(t *testing.T, r Result, want string) {
	t.Helper()
	if r.Err != nil || r.Text != want {
		t.Fatalf("got Text=%q Err=%v, want %q", r.Text, r.Err, want)
	}
}

// ---- tests ----

func TestExtractSimpleText(t *testing.T) {
	r := fresh(t, doc(helvetica, "", "BT /F1 12 Tf 72 720 Td (Hello World) Tj 0 -14 Td (Second line) Tj ET"))
	wantText(t, r, "Hello World\nSecond line")
	if r.Pages != 1 || r.Truncated {
		t.Fatalf("Pages=%d Truncated=%v", r.Pages, r.Truncated)
	}
}

func TestExtractWordSpacing(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"words placed apart", "BT /F1 10 Tf 72 700 Td (Hello) Tj 40 0 Td (World) Tj ET", "Hello World"},
		{"runs that touch", "BT /F1 10 Tf 72 700 Td (Hel) Tj (lo) Tj ET", "Hello"},
		{"TJ kerning vs word gap", "BT /F1 10 Tf 72 700 Td [(Hel) -20 (lo) -400 (there)] TJ ET", "Hello there"},
		{"separate text objects on one line", "BT /F1 10 Tf 72 700 Td (left) Tj ET BT /F1 10 Tf 200 700 Td (right) Tj ET", "left right"},
		{"font scaled by Tm", "BT /F1 1 Tf 12 0 0 12 72 700 Tm (one) Tj 0 -1.2 Td (two) Tj ET", "one\ntwo"},
		{"T* and quote operators", "BT /F1 10 Tf 14 TL 72 700 Td (a) Tj T* (b) Tj (c) ' ET", "a\nb\nc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantText(t, fresh(t, doc(helvetica, "", c.content)), c.want)
		})
	}
}

func TestExtractParagraphBreak(t *testing.T) {
	wantText(t, fresh(t, doc(helvetica, "", "BT /F1 10 Tf 72 700 Td (para one) Tj 0 -40 Td (para two) Tj ET")),
		"para one\n\npara two")
}

func TestExtractPageMarkers(t *testing.T) {
	r := fresh(t, doc(helvetica, "", "BT /F1 10 Tf 72 700 Td (first) Tj ET", "", "BT /F1 10 Tf 72 700 Td (third) Tj ET"))
	wantText(t, r, "[Page 1]\nfirst\n\n[Page 3]\nthird")
	if r.Pages != 3 {
		t.Fatalf("Pages = %d, want 3", r.Pages)
	}
}

func TestExtractCJKViaToUnicode(t *testing.T) {
	cmap := "/CIDInit /ProcSet findresource begin 12 dict begin begincmap\n" +
		"1 begincodespacerange <0000> <FFFF> endcodespacerange\n" +
		"2 beginbfchar <0001> <4E2D> <0002> <6587> endbfchar\n" +
		"1 beginbfrange <0010> <0012> <0041> endbfrange\n" +
		"endcmap CMapName currentdict /CMap defineresource pop end end"
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>")
	b.add("<< /Type /Font /Subtype /Type0 /BaseFont /X /Encoding /Identity-H /DescendantFonts [6 0 R] /ToUnicode 7 0 R >>")
	b.add(streamBody("", []byte("BT /F1 12 Tf 72 700 Td <00010002> Tj 0 -20 Td <001000110012> Tj ET"), true))
	b.add("<< /Type /Font /Subtype /CIDFontType2 /BaseFont /X /DW 1000 /W [16 [500 500 500]] >>")
	b.add(streamBody("", []byte(cmap), true))
	wantText(t, fresh(t, b.bytes()), "中文\nABC")
}

func TestExtractDifferencesEncoding(t *testing.T) {
	font := "<< /Type /Font /Subtype /Type1 /BaseFont /X /Encoding << /Differences [65 /alpha /beta] >> >>"
	wantText(t, fresh(t, doc(font, "", "BT /F1 12 Tf 72 700 Td (AB C) Tj ET")), "αβ C")
}

func TestExtractFormXObject(t *testing.T) {
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 4 0 R >> /XObject << /Fm1 6 0 R >> >> /Contents 5 0 R >>")
	b.add(helvetica)
	b.add(streamBody("", []byte("BT /F1 12 Tf 72 700 Td (before) Tj ET q /Fm1 Do Q BT /F1 12 Tf 72 600 Td (after) Tj ET"), true))
	// The form draws itself too; recursion must stop.
	b.add(streamBody("/Type /XObject /Subtype /Form /BBox [0 0 100 100] /Resources << /Font << /F1 4 0 R >> /XObject << /Fm1 6 0 R >> >>",
		[]byte("BT /F1 12 Tf 0 0 Td (inside form) Tj ET /Fm1 Do"), true))
	wantText(t, fresh(t, b.bytes()), "before\ninside form\nafter")
}

func TestExtractImageOnlyPage(t *testing.T) {
	r := fresh(t, doc(helvetica, "/XObject << /Im1 << /Type /XObject /Subtype /Image /Width 1 /Height 1 >> >>",
		"q 612 0 0 792 0 0 cm /Im1 Do Q"))
	if r.Text != "" || r.Err != nil || r.Pages != 1 {
		t.Fatalf("image-only page: %+v, want no text and no error", r)
	}
}

func TestExtractCorruptInputs(t *testing.T) {
	good := doc(helvetica, "", "BT /F1 12 Tf 72 700 Td (Hello) Tj ET")
	rng := rand.New(rand.NewSource(7))
	noise := make([]byte, 4096)
	rng.Read(noise)
	flipped := append([]byte(nil), good...)
	for i := 0; i < 40; i++ {
		flipped[9+rng.Intn(len(flipped)-40)] ^= byte(1 + rng.Intn(255))
	}
	inputs := map[string][]byte{
		"empty":       nil,
		"not a pdf":   []byte("hello, this is not a PDF at all"),
		"noise":       noise,
		"header only": []byte("%PDF-1.7\n%%EOF\n"),
		"truncated":   good[:len(good)/2],
		"bit flips":   flipped,
		"bad content": doc(helvetica, "", "BT /F1 12 Tf ) ] >> <<< (unterminated"),
	}
	for name, data := range inputs {
		t.Run(name, func(t *testing.T) {
			r := fresh(t, data) // must not panic or hang
			if r.Err == nil && r.Text == "" && name != "bit flips" && name != "bad content" {
				t.Fatalf("%s: no text and no error: %+v", name, r)
			}
		})
	}
}

func TestExtractKeepsGoodPagesAfterBadOne(t *testing.T) {
	r := fresh(t, doc(helvetica, "",
		"BT /F1 12 Tf 72 700 Td (kept) Tj ET ) broken",
		"BT /F1 12 Tf 72 700 Td (page two) Tj ET"))
	wantText(t, r, "[Page 1]\nkept\n\n[Page 2]\npage two")
}

func TestExtractBrokenToUnicodeFallsBack(t *testing.T) {
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 4 0 R /F2 6 0 R >> >> /Contents 5 0 R >>")
	b.add("<< /Type /Font /Subtype /Type1 /BaseFont /X /ToUnicode 7 0 R >>")
	b.add(streamBody("", []byte("BT /F1 12 Tf 72 700 Td (plain) Tj /F2 12 Tf 0 -20 Td (other font) Tj ET"), true))
	b.add(helvetica)
	b.add(streamBody("", []byte("endbfchar"), true)) // readCmap panics: missing beginbfchar
	wantText(t, fresh(t, b.bytes()), "plain\nother font")
}

func TestExtractDeadline(t *testing.T) {
	pages := make([]string, 300)
	for i := range pages {
		pages[i] = fmt.Sprintf("BT /F1 12 Tf 72 700 Td (page %d) Tj ET", i)
	}
	data := doc(helvetica, "", pages...)
	if r := extract(data, time.Now().Add(-time.Second)); !errors.Is(r.Err, ErrTooComplex) {
		t.Fatalf("expired deadline: got %+v", r)
	}
	if r := fresh(t, data); r.Err != nil || r.Pages != 300 {
		t.Fatalf("normal deadline: got Err=%v Pages=%d", r.Err, r.Pages)
	}
}

func TestExtractEncrypted(t *testing.T) {
	b := &pdfBuilder{trailer: "/Encrypt << /Filter /Custom >> /ID [<00> <00>]"}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [] /Count 0 >>")
	if r := fresh(t, b.bytes()); !errors.Is(r.Err, ErrEncrypted) {
		t.Fatalf("Err = %v, want ErrEncrypted", r.Err)
	}
}

func TestExtractTooLarge(t *testing.T) {
	if r := Extract(make([]byte, MaxInputBytes+1)); !errors.Is(r.Err, ErrTooLarge) {
		t.Fatalf("Err = %v, want ErrTooLarge", r.Err)
	}
}

func TestExtractTruncatesAtMaxChars(t *testing.T) {
	var c strings.Builder
	c.WriteString("BT /F1 10 Tf 12 TL 72 700 Td ")
	line := strings.Repeat("x", 99)
	for i := 0; i < MaxChars/100+500; i++ {
		fmt.Fprintf(&c, "(%s) Tj T* ", line)
	}
	c.WriteString("ET")
	r := fresh(t, doc(helvetica, "", c.String()))
	if r.Err != nil || !r.Truncated {
		t.Fatalf("Err=%v Truncated=%v, want a truncated result", r.Err, r.Truncated)
	}
	if n := utf8.RuneCountInString(r.Text); n != MaxChars {
		t.Fatalf("text has %d characters, want exactly %d", n, MaxChars)
	}
}

func TestExtractUndecodableFont(t *testing.T) {
	// Identity-H without a ToUnicode map: glyph ids, not text.
	font := "<< /Type /Font /Subtype /Type0 /BaseFont /X /Encoding /Identity-H /DescendantFonts [<< /Subtype /CIDFontType2 >>] >>"
	r := fresh(t, doc(font, "", "BT /F1 12 Tf 72 700 Td <0003000400050006000700080009> Tj ET"))
	if r.Text != "" || !errors.Is(r.Err, ErrUndecodable) {
		t.Fatalf("got %+v, want ErrUndecodable", r)
	}
}

func TestExtractSafetyLimitIsTooComplex(t *testing.T) {
	r := fresh(t, doc(helvetica, "", strings.Repeat("1 ", 70000)+"BT /F1 12 Tf (x) Tj ET"))
	if !errors.Is(r.Err, ErrTooComplex) || r.Text != "" {
		t.Fatalf("got %+v, want ErrTooComplex", r)
	}
}

func TestExtractDecompressionBomb(t *testing.T) {
	// 300 MiB of zeros (whitespace to the lexer) in ~300 KiB of Flate.
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	zeros := make([]byte, 1<<20)
	for i := 0; i < 300; i++ {
		w.Write(zeros)
	}
	w.Close()
	b := &pdfBuilder{}
	b.add("<< /Type /Catalog /Pages 2 0 R >>")
	b.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	b.add("<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>")
	b.add(fmt.Sprintf("<< /Filter /FlateDecode /Length %d >>\nstream\n%s\nendstream", z.Len(), z.Bytes()))
	if r := fresh(t, b.bytes()); !errors.Is(r.Err, ErrTooComplex) {
		t.Fatalf("got %+v, want ErrTooComplex", r)
	}
}

func TestExtractHostileStructuresTerminate(t *testing.T) {
	cyclic := &pdfBuilder{}
	cyclic.add("<< /Type /Catalog /Pages 2 0 R >>")
	cyclic.add("<< /Type /Pages /Kids [2 0 R 2 0 R] /Count 9 >>")

	// A ToUnicode CMap declaring three million entries but carrying one.
	bigCount := &pdfBuilder{}
	bigCount.add("<< /Type /Catalog /Pages 2 0 R >>")
	bigCount.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	bigCount.add("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>")
	bigCount.add("<< /Type /Font /Subtype /Type0 /Encoding /Identity-H /ToUnicode 6 0 R >>")
	bigCount.add(streamBody("", []byte("BT /F1 12 Tf <0041> Tj ET"), true))
	bigCount.add(streamBody("", []byte("1 begincodespacerange <0000> <FFFF> endcodespacerange 3000000 beginbfchar <0041> <0042> endbfchar"), true))

	// Two forms drawing each other.
	forms := &pdfBuilder{}
	forms.add("<< /Type /Catalog /Pages 2 0 R >>")
	forms.add("<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	forms.add("<< /Type /Page /Parent 2 0 R /Resources << /XObject << /A 4 0 R /B 5 0 R >> >> /Contents 6 0 R >>")
	forms.add(streamBody("/Subtype /Form /Resources << /XObject << /B 5 0 R >> >>", []byte("/B Do"), false))
	forms.add(streamBody("/Subtype /Form /Resources << /XObject << /A 4 0 R >> >>", []byte("/A Do"), false))
	forms.add(streamBody("", []byte("/A Do /B Do"), false))

	inputs := map[string][]byte{
		"cyclic page tree":         cyclic.bytes(),
		"hex string at end":        doc(helvetica, "", "BT /F1 12 Tf <4142"),
		"huge bfchar count":        bigCount.bytes(),
		"mutually recursive forms": forms.bytes(),
		"xref object 50 million":   bytes.Replace(doc(helvetica, "", "x"), []byte("trailer"), []byte("50000000 1\n0000000009 00000 n \ntrailer"), 1),
	}
	for name, data := range inputs {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			r := fresh(t, data)
			if d := time.Since(start); d > 10*time.Second {
				t.Fatalf("took %v", d)
			}
			if name == "huge bfchar count" && r.Text != "B" {
				t.Fatalf("Text = %q, want %q", r.Text, "B")
			}
			if name == "xref object 50 million" && !errors.Is(r.Err, ErrTooComplex) {
				t.Fatalf("Err = %v, want ErrTooComplex", r.Err)
			}
		})
	}
}

func TestExtractPDF20HeaderAndTrailingJunk(t *testing.T) {
	data := doc(helvetica, "", "BT /F1 12 Tf 72 700 Td (modern) Tj ET")
	v2 := append([]byte("%PDF-2.0"), data[len("%PDF-1.4"):]...)
	wantText(t, fresh(t, v2), "modern")
	junk := append(append([]byte(nil), data...), []byte("\x00\x00garbage after the end\r\n")...)
	wantText(t, fresh(t, junk), "modern")
}

func TestExtractCachesByContent(t *testing.T) {
	data := doc(helvetica, "", fmt.Sprintf("BT /F1 12 Tf 72 700 Td (cache %d) Tj ET", time.Now().UnixNano()))
	before := extractions.Load()
	first, second := Extract(data), Extract(data)
	if first.Text != second.Text || first.Text == "" {
		t.Fatalf("cached result differs: %q vs %q", first.Text, second.Text)
	}
	if n := extractions.Load() - before; n != 1 {
		t.Fatalf("parsed %d times, want 1", n)
	}
}

func TestExtractBusy(t *testing.T) {
	old := busyWait
	busyWait = 50 * time.Millisecond
	defer func() { busyWait = old }()
	for i := 0; i < cap(slots); i++ {
		slots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(slots); i++ {
			<-slots
		}
	}()
	data := doc(helvetica, "", fmt.Sprintf("BT (busy %d) Tj ET", time.Now().UnixNano()))
	if r := Extract(data); !errors.Is(r.Err, ErrBusy) {
		t.Fatalf("Err = %v, want ErrBusy", r.Err)
	}
}

func TestExtractDeterministic(t *testing.T) {
	data := doc(helvetica, "", "BT /F1 12 Tf 72 700 Td (same) Tj 30 0 Td (every) Tj 0 -30 Td (time) Tj ET")
	want := fresh(t, data)
	for i := 0; i < 5; i++ {
		if got := fresh(t, data); got != want {
			t.Fatalf("run %d: %+v, first run %+v", i, got, want)
		}
	}
}

func TestCleanPage(t *testing.T) {
	raw := "  \t\uFB01ne\u00A0print \x01\uFFFD\uE001 here\u00AD\n\n\n\nnext   line  \n"
	text, good, bad := cleanPage(raw)
	if text != "fine print here\n\nnext line" {
		t.Fatalf("text = %q", text)
	}
	if good != len("fineprinthere")-1+len("nextline") || bad != 3 {
		t.Fatalf("good=%d bad=%d", good, bad)
	}
}
