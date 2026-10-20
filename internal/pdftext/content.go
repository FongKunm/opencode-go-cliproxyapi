package pdftext

import (
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"opencode-go-cliproxyapi/internal/thirdparty/pdf"
)

type objRef struct {
	id  uint32
	gen uint16
}

func refOf(v pdf.Value) objRef {
	id, gen := v.Ref()
	return objRef{id, gen}
}

// pageRef is one leaf of the page tree with its inherited /Resources.
type pageRef struct {
	v   pdf.Value
	res pdf.Value
}

// collectPages walks the page tree once, in document order, inheriting
// /Resources down the tree. The walk is bounded in pages, nodes and depth
// and skips nodes already visited, so a cyclic tree terminates; unlike
// pdf.Reader.Page it costs O(nodes) instead of O(pages^2).
func collectPages(r *pdf.Reader) []pageRef {
	var pages []pageRef
	seen := make(map[objRef]bool)
	nodes := 0
	more := func() bool { return len(pages) < maxPages && nodes < maxPageNodes }
	var walk func(node, res pdf.Value, parent objRef, depth int)
	walk = func(node, res pdf.Value, parent objRef, depth int) {
		if node.Kind() != pdf.Dict || depth > maxTreeDepth || !more() {
			return
		}
		nodes++
		// Kids are indirect objects; a kid reporting its parent's reference
		// is an inline dictionary with no identity of its own.
		ref := refOf(node)
		if ref != parent {
			if seen[ref] {
				return
			}
			seen[ref] = true
		}
		if own := node.Key("Resources"); !own.IsNull() {
			res = own
		}
		kids := node.Key("Kids")
		if typ := node.Key("Type").Name(); typ == "Pages" || typ != "Page" && kids.Kind() == pdf.Array {
			for i := 0; i < kids.Len() && more(); i++ {
				walk(kids.Index(i), res, ref, depth+1)
			}
			return
		}
		pages = append(pages, pageRef{v: node, res: res})
	}
	walk(r.Trailer().Key("Root").Key("Pages"), pdf.Value{}, objRef{}, 0)
	return pages
}

// errPageFull stops a page whose raw text outgrew the page buffer.
var errPageFull = errors.New("page text buffer full")

type extractor struct {
	src         *source
	pages       int
	fonts       map[fontKey]*fontInfo
	widthBudget int
	ops         int

	doc         strings.Builder // finished pages
	docChars    int
	full        bool
	undecodable int

	pageBuf strings.Builder // raw text of the page in progress
	have    bool            // a run was shown since the last block break
	lastY   float64         // origin y of the last run (user space)
	lastX   float64         // x where the last run ended (user space)
}

func newExtractor(src *source, pages int) *extractor {
	return &extractor{src: src, pages: pages, fonts: make(map[fontKey]*fontInfo), widthBudget: maxWidthEntries}
}

// page extracts one page into the document. It reports false when the
// page's content could not be fully parsed; the text read up to that point
// is kept (the failure point is deterministic). Safety-limit panics end
// the whole document.
func (x *extractor) page(n int, pg pageRef) (ok bool) {
	x.pageBuf.Reset()
	x.have = false
	ok = true
	func() {
		defer func() {
			if p := recover(); p != nil {
				if _, limit := p.(*pdf.LimitError); limit {
					panic(p)
				}
				ok = p == errPageFull
			}
		}()
		if c := pg.v.Key("Contents"); !c.IsNull() {
			x.run(c, pg.res, 0, make(map[objRef]bool))
		}
	}()
	x.finishPage(n)
	return ok
}

func (x *extractor) finishPage(n int) {
	text, good, bad := cleanPage(x.pageBuf.String())
	if bad > good { // mostly glyphs without a Unicode mapping
		x.undecodable++
		return
	}
	if text == "" {
		return
	}
	head := ""
	if x.doc.Len() > 0 {
		head = "\n\n"
	}
	if x.pages > 1 {
		head += "[Page " + strconv.Itoa(n) + "]\n"
	}
	room := MaxChars - x.docChars - utf8.RuneCountInString(head)
	if room <= 0 {
		x.full = true
		return
	}
	if utf8.RuneCountInString(text) > room {
		text = cutRunes(text, room)
		x.full = true
	}
	x.doc.WriteString(head)
	x.doc.WriteString(text)
	x.docChars += utf8.RuneCountInString(head) + utf8.RuneCountInString(text)
}

func cutRunes(s string, n int) string {
	i := 0
	for k := range s {
		if i == n {
			return s[:k]
		}
		i++
	}
	return s
}

// raw appends to the page buffer, which holds at most 4*MaxChars bytes.
func (x *extractor) raw(s string) {
	if x.pageBuf.Len()+len(s) > 4*MaxChars {
		panic(errPageFull)
	}
	x.pageBuf.WriteString(s)
}

// blockBreak ends the current line and forgets the last position, for
// content drawn in another coordinate space (form XObjects).
func (x *extractor) blockBreak() {
	if x.have {
		x.raw("\n")
	}
	x.have = false
}

func (x *extractor) tick() {
	x.ops++
	if x.ops > maxOperators {
		panic(&pdf.LimitError{What: "content operators"})
	}
	if x.ops&1023 == 0 {
		x.src.checkTime()
	}
}

// ---- content streams ----

type matrix [6]float64 // a b c d e f

var identity = matrix{1, 0, 0, 1, 0, 0}

type textState struct {
	font   *fontInfo
	size   float64
	tc, tw float64 // character and word spacing
	th     float64 // horizontal scaling, 1 = 100%
	tl     float64 // leading
	tm     matrix  // text matrix
	tlm    matrix  // text line matrix
}

func (s *textState) translate(tx, ty float64) {
	m := s.tlm
	m[4] += tx*m[0] + ty*m[2]
	m[5] += tx*m[1] + ty*m[3]
	s.tlm, s.tm = m, m
}

// operands returns the last n operands, or nil when fewer are present.
func operands(args []pdf.Value, n int) []pdf.Value {
	if len(args) < n {
		return nil
	}
	return args[len(args)-n:]
}

func popArgs(stk *pdf.Stack) []pdf.Value {
	args := make([]pdf.Value, stk.Len())
	for i := len(args) - 1; i >= 0; i-- {
		args[i] = stk.Pop()
	}
	return args
}

// run interprets one content stream (a page or a form XObject) and feeds
// the text it shows to the page buffer. Only the text-related operators
// matter; graphics state is ignored, so positions are compared in text
// space within one stream.
func (x *extractor) run(content, res pdf.Value, depth int, forms map[objRef]bool) {
	st := &textState{th: 1, tm: identity, tlm: identity}
	pdf.Interpret(content, func(stk *pdf.Stack, op string) {
		x.tick()
		args := popArgs(stk)
		num := func(i, n int) float64 { return operands(args, n)[i].Float64() }
		switch op {
		case "BT":
			st.tm, st.tlm = identity, identity
		case "Tf":
			if a := operands(args, 2); a != nil {
				st.font = x.font(res, a[0].Name())
				st.size = a[1].Float64()
			}
		case "Tc", "Tw", "Tz", "TL":
			if len(args) == 0 {
				return
			}
			v := num(0, 1)
			switch op {
			case "Tc":
				st.tc = v
			case "Tw":
				st.tw = v
			case "Tz":
				st.th = v / 100
			default:
				st.tl = v
			}
		case "Td", "TD":
			if len(args) < 2 {
				return
			}
			if op == "TD" {
				st.tl = -num(1, 2)
			}
			st.translate(num(0, 2), num(1, 2))
		case "Tm":
			if a := operands(args, 6); a != nil {
				for i := range st.tlm {
					st.tlm[i] = a[i].Float64()
				}
				st.tm = st.tlm
			}
		case "T*":
			st.translate(0, -st.tl)
		case "Tj":
			if a := operands(args, 1); a != nil {
				x.show(st, a[0].RawString())
			}
		case "'":
			st.translate(0, -st.tl)
			if a := operands(args, 1); a != nil {
				x.show(st, a[0].RawString())
			}
		case `"`:
			if a := operands(args, 3); a != nil {
				st.tw, st.tc = a[0].Float64(), a[1].Float64()
				st.translate(0, -st.tl)
				x.show(st, a[2].RawString())
			}
		case "TJ":
			if a := operands(args, 1); a != nil {
				for i := 0; i < a[0].Len(); i++ {
					switch el := a[0].Index(i); el.Kind() {
					case pdf.String:
						x.show(st, el.RawString())
					case pdf.Integer, pdf.Real:
						st.kern(el.Float64())
					}
				}
			}
		case "Do":
			if a := operands(args, 1); a != nil && depth < maxFormDepth {
				x.form(a[0].Name(), res, depth, forms)
			}
		}
	})
}

// show appends one text run, separating it from the previous run by a
// line break when the baseline moved, or by a space when there is a
// visible horizontal gap, then advances the text matrix by the run width.
func (x *extractor) show(st *textState, raw string) {
	if raw == "" {
		return
	}
	text := st.font.decode(raw)
	m := st.tm
	px, py := m[4], m[5]
	fs := math.Abs(st.size) * math.Hypot(m[2], m[3]) // font size in user space
	if fs < 1e-3 {
		fs = math.Max(math.Abs(st.size), 1)
	}
	if x.have {
		dy := math.Abs(py - x.lastY)
		gap := px - x.lastX
		switch {
		case dy > 2*fs:
			x.raw("\n\n")
		case dy > 0.5*fs:
			x.raw("\n")
		case gap > 0.15*fs || gap < -0.5*fs:
			x.raw(" ")
		}
	}
	x.raw(text)
	adv := st.advance(raw)
	st.tm[4] += adv * m[0]
	st.tm[5] += adv * m[1]
	x.have, x.lastY, x.lastX = true, py, st.tm[4]
}

// kern applies one TJ adjustment (thousandths of a text-space unit). A
// wide negative adjustment is how many producers write a word space; it
// shows up as a gap before the next run.
func (st *textState) kern(n float64) {
	tx := -n / 1000 * st.size * st.th
	st.tm[4] += tx * st.tm[0]
	st.tm[5] += tx * st.tm[1]
}

// advance returns the horizontal displacement of a run in text space.
func (st *textState) advance(raw string) float64 {
	var w float64
	codes, spaces := 0, 0
	if f := st.font; f != nil && f.twoByte {
		for i := 0; i+1 < len(raw); i += 2 {
			w += f.cidWidth(int(raw[i])<<8 | int(raw[i+1]))
			codes++
		}
	} else {
		for i := 0; i < len(raw); i++ {
			w += st.font.width(raw[i])
			codes++
			if raw[i] == ' ' {
				spaces++
			}
		}
	}
	return (w/1000*st.size + st.tc*float64(codes) + st.tw*float64(spaces)) * st.th
}

func (x *extractor) form(name string, res pdf.Value, depth int, forms map[objRef]bool) {
	xo := res.Key("XObject").Key(name)
	if xo.Kind() != pdf.Stream || xo.Key("Subtype").Name() != "Form" {
		return
	}
	ref := refOf(xo)
	if forms[ref] { // a form drawing itself
		return
	}
	forms[ref] = true
	defer delete(forms, ref)
	fres := xo.Key("Resources")
	if fres.IsNull() {
		fres = res
	}
	x.blockBreak()
	x.run(xo, fres, depth+1, forms)
	x.blockBreak()
}

// ---- fonts ----

type fontKey struct {
	ref  objRef
	name string
}

type widthSeg struct {
	first, last int
	w           float64   // width of every CID in the segment, when ws is nil
	ws          []float32 // per-CID widths from first
}

// fontInfo is what extraction needs from one font: a decoder to Unicode
// and glyph widths (thousandths of an em) to place runs on a line.
type fontInfo struct {
	enc     pdf.TextEncoding
	twoByte bool      // Type0: 2-byte codes (Identity-H, UCS-2 CMaps)
	first   int       // simple fonts: first code in widths
	widths  []float64 // simple fonts
	missing float64   // simple fonts: codes outside widths
	dw      float64   // Type0: default width
	segs    []widthSeg
}

func (f *fontInfo) decode(raw string) string {
	if f == nil {
		return latin1{}.Decode(raw)
	}
	return f.enc.Decode(raw)
}

func (f *fontInfo) width(code byte) float64 {
	if f == nil {
		return 500
	}
	if i := int(code) - f.first; i >= 0 && i < len(f.widths) {
		return f.widths[i]
	}
	return f.missing
}

func (f *fontInfo) cidWidth(cid int) float64 {
	k := sort.Search(len(f.segs), func(i int) bool { return f.segs[i].last >= cid })
	if k < len(f.segs) && f.segs[k].first <= cid {
		s := f.segs[k]
		if s.ws != nil {
			return float64(s.ws[cid-s.first])
		}
		return s.w
	}
	return f.dw
}

// latin1 decodes bytes one-to-one; used without a font or when a font's
// encoding cannot be read.
type latin1 struct{}

func (latin1) Decode(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for i := 0; i < len(raw); i++ {
		b.WriteRune(rune(raw[i]))
	}
	return b.String()
}

func isNumber(v pdf.Value) bool {
	return v.Kind() == pdf.Integer || v.Kind() == pdf.Real
}

func (x *extractor) font(res pdf.Value, name string) *fontInfo {
	fv := res.Key("Font").Key(name)
	if fv.Kind() != pdf.Dict {
		return nil
	}
	key := fontKey{refOf(fv), name}
	if f, ok := x.fonts[key]; ok {
		return f
	}
	if len(x.fonts) >= maxFonts {
		panic(&pdf.LimitError{What: "font count"})
	}
	f := &fontInfo{enc: encoderOf(fv), missing: 500, dw: 1000}
	if fv.Key("Subtype").Name() == "Type0" {
		f.twoByte = true
		desc := fv.Key("DescendantFonts").Index(0)
		if dw := desc.Key("DW"); isNumber(dw) {
			f.dw = dw.Float64()
		}
		f.segs = x.parseW(desc.Key("W"))
	} else {
		f.first = int(fv.Key("FirstChar").Int64())
		ws := fv.Key("Widths")
		f.widths = make([]float64, min(ws.Len(), 256))
		for i := range f.widths {
			f.widths[i] = ws.Index(i).Float64()
		}
		if mw := fv.Key("FontDescriptor").Key("MissingWidth"); isNumber(mw) && mw.Float64() > 0 {
			f.missing = mw.Float64()
		}
	}
	x.fonts[key] = f
	return f
}

// encoderOf returns the font's decoder; a font whose encoding or
// ToUnicode CMap cannot be parsed decodes bytes one-to-one instead of
// failing the page.
func encoderOf(fv pdf.Value) (enc pdf.TextEncoding) {
	defer func() {
		if p := recover(); p != nil {
			if _, limit := p.(*pdf.LimitError); limit {
				panic(p)
			}
			enc = latin1{}
		}
	}()
	f := pdf.Font{V: fv}
	return f.Encoder()
}

// parseW reads a CIDFont /W array ([c [w1 w2 ...]] and [cfirst clast w]
// entries) into sorted segments, charging the document's width budget.
func (x *extractor) parseW(w pdf.Value) []widthSeg {
	var segs []widthSeg
	n := w.Len()
	for i := 0; i+1 < n && x.widthBudget > 0; {
		first := w.Index(i)
		if first.Kind() != pdf.Integer {
			break
		}
		c := int(first.Int64())
		next := w.Index(i + 1)
		x.widthBudget -= 8 // segment overhead
		if next.Kind() == pdf.Array {
			ws := make([]float32, max(0, min(next.Len(), x.widthBudget)))
			for j := range ws {
				ws[j] = float32(next.Index(j).Float64())
			}
			x.widthBudget -= len(ws)
			if len(ws) > 0 {
				segs = append(segs, widthSeg{first: c, last: c + len(ws) - 1, ws: ws})
			}
			i += 2
			continue
		}
		if i+2 >= n {
			break
		}
		if last := int(next.Int64()); last >= c {
			segs = append(segs, widthSeg{first: c, last: last, w: w.Index(i + 2).Float64()})
		}
		i += 3
	}
	sort.Slice(segs, func(a, b int) bool { return segs[a].first < segs[b].first })
	return segs
}
