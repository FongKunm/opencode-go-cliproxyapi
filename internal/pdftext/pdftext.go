// Package pdftext turns PDF bytes into plain text for models that cannot
// read PDF files (see internal/adapter/sanitize).
//
// The PDFs come from client requests and are parsed inside the host
// process, so extraction is bounded on every axis: input size, output
// size, decompressed bytes, parser work, raw reads and wall time (the
// last one only as a backstop). It never panics; anything unreadable is
// reported through Result.Err. Results are cached by content because
// agent clients resend the same document on every turn, and the result for
// a given file is deterministic except under the time backstop.
package pdftext

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"opencode-go-cliproxyapi/internal/thirdparty/pdf"
)

const (
	// MaxInputBytes is the largest PDF Extract reads (decoded bytes; the
	// same limit Anthropic applies to a PDF request).
	MaxInputBytes = 32 << 20
	// MaxChars caps the extracted text, in characters.
	MaxChars = 200_000

	maxPages        = 2_000
	maxPageNodes    = 20_000
	maxTreeDepth    = 64
	maxFonts        = 4_096
	maxWidthEntries = 1 << 21 // CID widths stored across all fonts
	maxFormDepth    = 4
	maxOperators    = 20_000_000
	maxRawReads     = 2 << 30 // bytes the parser may copy out of the file
	timeLimit       = 10 * time.Second

	concurrency  = 2
	cacheEntries = 16
)

// busyWait is how long Extract waits for a free slot (a variable for tests).
var busyWait = 10 * time.Second

// Reasons a PDF yields no text (Result.Err).
var (
	ErrTooLarge    = errors.New("the PDF exceeds the size limit")
	ErrEncrypted   = errors.New("the PDF is encrypted")
	ErrMalformed   = errors.New("the PDF could not be parsed")
	ErrTooComplex  = errors.New("the PDF exceeds the processing limits")
	ErrUndecodable = errors.New("the PDF text uses fonts without a Unicode mapping")
	ErrBusy        = errors.New("PDF text extraction is busy")
)

// Result is the outcome of one extraction.
type Result struct {
	// Text is the extracted text with page markers ("[Page N]") when the
	// document has more than one page. Empty when nothing was extractable.
	Text string
	// Pages is the number of pages found in the page tree.
	Pages int
	// Truncated reports that Text stops at MaxChars.
	Truncated bool
	// Err explains why there is no text; nil with an empty Text means the
	// PDF has no text layer (a scanned or image-only document).
	Err error
}

// Extract returns the text of a PDF. It is safe for concurrent use.
func Extract(data []byte) Result {
	if len(data) > MaxInputBytes {
		return Result{Err: ErrTooLarge}
	}
	key := sha256.Sum256(data)
	if res, ok := results.get(key); ok {
		return res
	}
	if !acquire() {
		return Result{Err: ErrBusy}
	}
	defer release()
	if res, ok := results.get(key); ok { // filled while we waited
		return res
	}
	res := extract(data, time.Now().Add(timeLimit))
	results.put(key, res)
	return res
}

// ---- concurrency and cache ----

var slots = make(chan struct{}, concurrency)

// acquire bounds concurrent extractions, so a burst of large documents
// cannot multiply the per-document memory bound.
func acquire() bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
	}
	t := time.NewTimer(busyWait)
	defer t.Stop()
	select {
	case slots <- struct{}{}:
		return true
	case <-t.C:
		return false
	}
}

func release() { <-slots }

type resultCache struct {
	mu    sync.Mutex
	order *list.List // front = most recently used
	byKey map[[32]byte]*list.Element
}

type cacheEntry struct {
	key [32]byte
	res Result
}

var results = &resultCache{order: list.New(), byKey: make(map[[32]byte]*list.Element)}

func (c *resultCache) get(key [32]byte) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byKey[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*cacheEntry).res, true
	}
	return Result{}, false
}

func (c *resultCache) put(key [32]byte, res Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byKey[key]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.byKey[key] = c.order.PushFront(&cacheEntry{key: key, res: res})
	for c.order.Len() > cacheEntries {
		el := c.order.Back()
		c.order.Remove(el)
		delete(c.byKey, el.Value.(*cacheEntry).key)
	}
}

// extractions counts parses (cache misses); tests use it.
var extractions atomic.Int64

// ---- extraction ----

// source serves the PDF bytes to the parser and enforces the raw-read and
// time budgets. Every object the parser touches is read through it, so a
// loop in the parser cannot outlive these checks.
type source struct {
	data     []byte
	header   []byte // replaces the first bytes of data when non-nil
	read     int64
	calls    uint64
	deadline time.Time
}

// newSource prepares data for the parser, which insists on a "%PDF-1.x"
// header and on "%%EOF" being the last thing in the file: PDF 2.0 headers
// are presented as 1.7 (the syntax the parser reads is the same) and junk
// after the last "%%EOF" is cut off.
func newSource(data []byte, deadline time.Time) *source {
	s := &source{data: data, deadline: deadline}
	if bytes.HasPrefix(data, []byte("%PDF-2.")) && len(data) > 8 {
		s.header = []byte("%PDF-1.7")
	}
	if i := bytes.LastIndex(data, []byte("%%EOF")); i >= 0 {
		end := i + len("%%EOF")
		if len(bytes.TrimSpace(data[end:])) > 0 {
			s.data = data[:end]
		}
	}
	return s
}

func (s *source) ReadAt(p []byte, off int64) (int, error) {
	s.calls++
	if s.calls%64 == 0 {
		s.checkTime()
	}
	s.read += int64(len(p))
	if s.read > maxRawReads {
		panic(&pdf.LimitError{What: "raw read volume"})
	}
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= int64(len(s.data)) {
		return 0, io.EOF
	}
	n := copy(p, s.data[off:])
	if off < int64(len(s.header)) {
		copy(p[:n], s.header[off:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// checkTime panics with a *pdf.LimitError past the deadline. The parser
// lets limit errors propagate (see thirdparty/pdf/hardening.go).
func (s *source) checkTime() {
	if time.Now().After(s.deadline) {
		panic(&pdf.LimitError{What: "processing time"})
	}
}

func extract(data []byte, deadline time.Time) (res Result) {
	extractions.Add(1)
	defer func() {
		if p := recover(); p != nil {
			res = Result{Err: classifyPanic(p)}
		}
	}()
	src := newSource(data, deadline)
	r, err := pdf.NewReader(src, int64(len(src.data)))
	if err != nil {
		return Result{Err: classifyOpenErr(err)}
	}
	pages := collectPages(r)
	x := newExtractor(src, len(pages))
	failed := 0
	for i, pg := range pages {
		if !x.page(i+1, pg) {
			failed++
		}
		if x.full {
			break
		}
	}
	res = Result{Text: x.doc.String(), Pages: len(pages), Truncated: x.full}
	if res.Text == "" {
		switch {
		case x.undecodable > 0:
			res.Err = ErrUndecodable
		case failed > 0:
			res.Err = ErrMalformed
		}
	}
	return res
}

func classifyOpenErr(err error) error {
	var le *pdf.LimitError
	msg := err.Error()
	switch {
	case errors.As(err, &le), strings.Contains(msg, "safety limit"):
		return ErrTooComplex
	case errors.Is(err, pdf.ErrInvalidPassword), strings.Contains(msg, "encrypt"):
		return ErrEncrypted
	default:
		return ErrMalformed
	}
}

func classifyPanic(p any) error {
	if _, ok := p.(*pdf.LimitError); ok {
		return ErrTooComplex
	}
	return ErrMalformed
}

// ---- text cleanup ----

var ligatures = map[rune]string{
	0xFB00: "ff", 0xFB01: "fi", 0xFB02: "fl", 0xFB03: "ffi",
	0xFB04: "ffl", 0xFB05: "st", 0xFB06: "st",
}

// cleanPage normalizes one page of raw extracted text: it drops control
// characters, unmapped glyphs (U+FFFD, private use) and invisible marks,
// expands ligatures, collapses runs of blanks, trims lines and keeps at
// most one blank line in a row. good and bad count the visible characters
// kept and the unmapped ones dropped, to spot fonts that decode to junk.
func cleanPage(raw string) (text string, good, bad int) {
	var b strings.Builder
	b.Grow(len(raw))
	newlines := 0 // line breaks seen since the last visible character
	space := false
	started := false
	for _, r := range raw {
		switch {
		case r == '\n':
			newlines++
		case r == ' ' || r == '\t' || r == 0xA0: // 0xA0: no-break space
			space = true
		case r == utf8.RuneError || r >= 0xE000 && r <= 0xF8FF: // unmapped glyphs, private use
			bad++
		case r < 0x20 || r == 0x7F || r >= 0x80 && r < 0xA0:
			bad++
		case r == 0xAD || r >= 0x200B && r <= 0x200D || r == 0xFEFF:
			// invisible: soft hyphen, zero-width characters, BOM
		default:
			good++
			if started {
				switch {
				case newlines >= 2:
					b.WriteString("\n\n")
				case newlines == 1:
					b.WriteByte('\n')
				case space:
					b.WriteByte(' ')
				}
			}
			newlines, space, started = 0, false, true
			if s, ok := ligatures[r]; ok {
				b.WriteString(s)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String(), good, bad
}
