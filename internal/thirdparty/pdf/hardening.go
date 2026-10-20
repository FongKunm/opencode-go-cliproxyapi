package pdf

import (
	"io"
	"sort"
)

// opencode-go hardening: resource limits for untrusted input.
//
// Upstream trusts the numbers a file declares (object counts, field
// widths, CMap entry counts) and loops until input runs out. That is fine
// for a command-line tool but not inside a long-lived server process: one
// hostile or merely truncated PDF could spin a goroutine forever or make the
// Go runtime die with an out-of-memory error that recover cannot catch.
// Each limit below closes one such path. Exceeding a hard limit panics with
// a *LimitError: NewReader returns it as an error, and callers of the Value
// API recover it like the package's other parse panics. Soft limits
// (token length, CMap entry count) truncate silently instead.
const (
	// maxXrefEntries bounds the cross-reference table; object numbers at
	// or above it are rejected instead of growing the table.
	maxXrefEntries = 1 << 20
	// maxXrefFieldWidth bounds each /W entry of an xref stream, in bytes.
	maxXrefFieldWidth = 8
	// maxXrefFields bounds the number of /W entries of an xref stream.
	maxXrefFields = 16
	// maxPredictorColumns bounds /DecodeParms /Columns of PNG predictors.
	maxPredictorColumns = 1 << 16
	// maxFilterChain bounds the number of filters applied to one stream.
	maxFilterChain = 8
	// defaultInflateLimit is the decompressed-byte budget of one Reader.
	defaultInflateLimit = 256 << 20
	// maxTokenBytes truncates one string, name or keyword token (soft).
	maxTokenBytes = 1 << 20
	// maxObjectElements bounds the array and dictionary members of one
	// top-level object, nested members included.
	maxObjectElements = 1 << 18
	// maxStackDepth bounds the Interpret operand stack.
	maxStackDepth = 1 << 16
	// maxDictStack bounds Interpret begin/end nesting.
	maxDictStack = 64
	// maxDictDefs bounds the def operations of one Interpret call.
	maxDictDefs = 1 << 16
	// maxCmapEntries bounds the codespace/bfchar/bfrange entries parsed
	// per Reader (soft: further entries are ignored).
	maxCmapEntries = 1 << 18
	// maxCodespaceRanges bounds the codespace ranges of one CMap per code
	// length (soft).
	maxCodespaceRanges = 64
	// maxCmapWork bounds the comparisons CMap decoding may spend per
	// Reader; well-formed CMaps use a handful per character.
	maxCmapWork = 1 << 28
	// maxPageTreeSteps bounds the page-tree nodes one Page lookup visits.
	maxPageTreeSteps = 1 << 16
	// maxInheritDepth bounds the /Parent links findInherited follows.
	maxInheritDepth = 64
)

// LimitError reports that a PDF exceeded one of the hardening limits.
type LimitError struct {
	What string
}

func (e *LimitError) Error() string {
	return "pdf: " + e.What + " exceeds the safety limit"
}

func limitPanic(what string) {
	panic(&LimitError{What: what})
}

// SetInflateLimit sets the number of decompressed bytes r may produce
// across all streams it decodes (default 256 MiB). Reading past the budget
// panics with a *LimitError. n <= 0 restores the default.
func (r *Reader) SetInflateLimit(n int64) {
	r.inflateLimit = n
}

// inflateBudget charges every decoded byte to the owning Reader.
type inflateBudget struct {
	rd io.Reader
	r  *Reader
}

func (b *inflateBudget) Read(p []byte) (int, error) {
	limit := b.r.inflateLimit
	if limit <= 0 {
		limit = defaultInflateLimit
	}
	if b.r.inflated >= limit {
		limitPanic("decompressed data")
	}
	if rest := limit - b.r.inflated; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := b.rd.Read(p)
	b.r.inflated += int64(n)
	return n, err
}

// appendToken appends bytes to a token under construction, dropping them
// once the token reaches maxTokenBytes. The lexer still consumes the rest
// of the token, so parsing stays aligned while memory stays bounded.
func appendToken(tmp []byte, c ...byte) []byte {
	if len(tmp)+len(c) > maxTokenBytes {
		return tmp
	}
	return append(tmp, c...)
}

// countElem charges one array/dictionary member to the top-level object
// being parsed.
func (b *buffer) countElem() {
	b.elems++
	if b.elems > maxObjectElements {
		limitPanic("object size")
	}
}

// addCmapEntry reports whether one more CMap entry fits the Reader's
// budget, charging it when it does. CMaps built without a Reader (tests)
// are unbounded.
func (m *cmap) addCmapEntry() bool {
	if m.r == nil {
		return true
	}
	if m.r.cmapEntries >= maxCmapEntries {
		return false
	}
	m.r.cmapEntries++
	return true
}

// chargeCmapWork spends n comparisons of the Reader's decoding budget.
func (m *cmap) chargeCmapWork(n int) {
	if m.r == nil {
		return
	}
	m.r.cmapWork += int64(n)
	if m.r.cmapWork > maxCmapWork {
		limitPanic("text decoding work")
	}
}

// cmapIndex accelerates cmap.Decode without changing its result: bfchar
// lookups use a map holding the first entry per code (Decode scans bfchar
// in order and stops at the first match), and bfrange lookups binary search
// per code length when that length's ranges do not overlap — then at most
// one range can contain a code, so the first match is the only match.
// Overlapping ranges fall back to the original ordered scan.
type cmapIndex struct {
	chars  map[string]string
	ranges [4][]int // bfrange indexes per code length, sorted by lo
	sorted [4]bool  // ranges[n] is non-overlapping and searchable
}

func (m *cmap) index() *cmapIndex {
	if m.idx != nil {
		return m.idx
	}
	idx := &cmapIndex{chars: make(map[string]string, len(m.bfchar))}
	for _, c := range m.bfchar {
		if _, ok := idx.chars[c.orig]; !ok {
			idx.chars[c.orig] = c.repl
		}
	}
	for i, br := range m.bfrange {
		if n := len(br.lo); n >= 1 && n <= 4 {
			idx.ranges[n-1] = append(idx.ranges[n-1], i)
		}
	}
	for n := range idx.ranges {
		list := idx.ranges[n]
		sort.SliceStable(list, func(a, b int) bool {
			return m.bfrange[list[a]].lo < m.bfrange[list[b]].lo
		})
		ok := true
		for k := 1; k < len(list); k++ {
			if m.bfrange[list[k-1]].hi >= m.bfrange[list[k]].lo {
				ok = false
				break
			}
		}
		idx.sorted[n] = ok
	}
	m.idx = idx
	return idx
}

// findRange returns the first bfrange (in declaration order) containing
// text, or nil.
func (m *cmap) findRange(text string) *bfrange {
	idx := m.index()
	n := len(text)
	if n < 1 || n > 4 {
		return nil
	}
	list := idx.ranges[n-1]
	if idx.sorted[n-1] {
		m.chargeCmapWork(1)
		k := sort.Search(len(list), func(i int) bool { return m.bfrange[list[i]].lo > text }) - 1
		if k >= 0 && text <= m.bfrange[list[k]].hi {
			return &m.bfrange[list[k]]
		}
		return nil
	}
	m.chargeCmapWork(len(m.bfrange))
	for i := range m.bfrange {
		br := &m.bfrange[i]
		if len(br.lo) == n && br.lo <= text && text <= br.hi {
			return br
		}
	}
	return nil
}
