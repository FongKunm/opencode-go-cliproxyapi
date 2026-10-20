package sanitize

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"opencode-go-cliproxyapi/internal/pdftext"
)

// maxDocChars caps the text kept from one document or file, the same cap
// PDF extraction uses.
const maxDocChars = pdftext.MaxChars

// doc is a document or file input, independent of protocol.
type doc struct {
	title     string
	context   string
	mediaType string
	b64       string // base64 payload
	text      string // inline text
	hasText   bool
	url       string
	fileID    string
	images    []json.RawMessage // image blocks inside a content-source document
}

// render returns the text the model sees in place of the document.
func (d doc) render() string {
	var body, note string
	pdfPages := -1
	truncated := false
	switch {
	case d.hasText:
		body = d.text
	case d.b64 != "" && isPDF(d.mediaType):
		body, note, pdfPages, truncated = pdfText(d.b64)
	case d.b64 != "" && isTextMedia(d.mediaType):
		var ok bool
		body, truncated, ok = base64Text(d.b64)
		if !ok {
			body, note = "", "(The file content could not be decoded as UTF-8 text.)"
		}
	case d.b64 != "":
		note = "(" + mediaLabel(d.mediaType) + " content is not supported here.)"
	case d.url != "":
		note = "(The document is at " + d.url + " and was not fetched.)"
	case d.fileID != "":
		note = "(The document is an uploaded file (" + d.fileID + ") that is not available here.)"
	case len(d.images) == 0:
		note = "(The document has no readable content.)"
	}
	if n := utf8.RuneCountInString(body); n > maxDocChars {
		body = cutRunes(body, maxDocChars)
		truncated = true
	}

	var b strings.Builder
	b.WriteString("[Document")
	if d.title != "" {
		b.WriteString(": " + d.title)
	}
	switch {
	case pdfPages == 1:
		b.WriteString(" (PDF, 1 page)")
	case pdfPages > 1:
		b.WriteString(" (PDF, " + strconv.Itoa(pdfPages) + " pages)")
	case pdfPages == 0:
		b.WriteString(" (PDF)")
	}
	b.WriteString("]")
	if d.context != "" {
		b.WriteString("\nContext: " + d.context)
	}
	if note != "" {
		b.WriteString("\n" + note)
	}
	if body != "" {
		b.WriteString("\n" + body)
	}
	if truncated {
		b.WriteString("\n[Document truncated: only the first " + strconv.Itoa(maxDocChars) + " characters are included.]")
	}
	return b.String()
}

// pdfText extracts a base64 PDF. pages is the page count (0 when the file
// could not be read).
func pdfText(b64 string) (body, note string, pages int, truncated bool) {
	data, ok := decodeBase64(b64, pdftext.MaxInputBytes)
	if !ok {
		if base64.StdEncoding.DecodedLen(len(b64)) > pdftext.MaxInputBytes {
			return "", "(The PDF was not read: it exceeds the size limit.)", 0, false
		}
		return "", "(The PDF data could not be decoded.)", 0, false
	}
	res := pdftext.Extract(data)
	switch {
	case errors.Is(res.Err, pdftext.ErrTooLarge):
		return "", "(The PDF was not read: it exceeds the size limit.)", 0, false
	case res.Err != nil:
		return "", "(The PDF text could not be extracted: " + res.Err.Error() + ".)", res.Pages, false
	case res.Text == "":
		return "", "(This PDF has no extractable text; it may be a scanned or image-only document.)", res.Pages, false
	}
	return res.Text, "", res.Pages, res.Truncated
}

// decodeBase64 decodes standard or URL-safe base64, padded or not,
// ignoring line breaks. Payloads decoding to more than limit bytes are
// rejected before decoding.
func decodeBase64(s string, limit int) ([]byte, bool) {
	s = stripBreaks(s)
	if base64.StdEncoding.DecodedLen(len(s)) > limit+3 {
		return nil, false
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// stripBreaks removes the line breaks and blanks some encoders insert into
// base64.
func stripBreaks(s string) string {
	if !strings.ContainsAny(s, "\r\n \t") {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch r {
		case '\r', '\n', ' ', '\t':
			return -1
		}
		return r
	}, s)
}

// base64Text decodes base64 text, reading only as much of a large payload
// as the character cap can use (UTF-8 is at most 4 bytes a character).
func base64Text(b64 string) (text string, truncated, ok bool) {
	const maxBytes = maxDocChars * 4
	b64 = stripBreaks(b64)
	if limit := maxBytes / 3 * 4; len(b64) > limit { // whole base64 quanta only
		b64 = b64[:limit]
		truncated = true
	}
	data, ok := decodeBase64(b64, maxBytes)
	if !ok {
		return "", false, false
	}
	if truncated { // the cut may split the last character (at most 3 bytes)
		for i := 0; i < 3 && len(data) > 0 && !utf8.Valid(data); i++ {
			data = data[:len(data)-1]
		}
	}
	if !utf8.Valid(data) {
		return "", false, false
	}
	return string(data), truncated, true
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

// baseMediaType lower-cases a media type and drops its parameters.
func baseMediaType(mt string) string {
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	return strings.ToLower(strings.TrimSpace(mt))
}

func isPDF(mt string) bool { return baseMediaType(mt) == "application/pdf" }

func isTextMedia(mt string) bool {
	mt = baseMediaType(mt)
	switch {
	case strings.HasPrefix(mt, "text/"),
		strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	switch mt {
	case "application/json", "application/xml", "application/javascript", "application/x-javascript",
		"application/yaml", "application/x-yaml", "application/toml", "application/x-sh",
		"application/sql", "application/csv", "application/x-ndjson":
		return true
	}
	return false
}

func mediaLabel(mt string) string {
	if mt = baseMediaType(mt); mt != "" {
		return mt
	}
	return "Binary"
}

// mediaTypeFromName guesses a media type from a file name, for file inputs
// that carry raw base64 without a data: URL. A fixed table keeps the
// result the same on every host.
func mediaTypeFromName(name string) string {
	ext := strings.ToLower(name)
	if i := strings.LastIndexByte(ext, '.'); i >= 0 {
		ext = ext[i+1:]
	} else {
		return ""
	}
	switch ext {
	case "pdf":
		return "application/pdf"
	case "json":
		return "application/json"
	case "xml":
		return "application/xml"
	case "yaml", "yml":
		return "application/yaml"
	case "txt", "md", "markdown", "csv", "tsv", "log", "html", "htm", "css", "js", "ts", "tsx", "jsx",
		"go", "py", "java", "c", "h", "cc", "cpp", "hpp", "rs", "rb", "php", "sh", "sql", "toml", "ini", "cfg":
		return "text/plain"
	}
	return ""
}

// claudeDocument reads an Anthropic document block (also the document
// inside a web_fetch result).
func claudeDocument(m map[string]json.RawMessage) doc {
	d := doc{title: str(m, "title"), context: str(m, "context")}
	src, _ := object(m["source"])
	d.mediaType = str(src, "media_type")
	switch str(src, "type") {
	case "text":
		d.text, d.hasText = str(src, "data"), true
	case "base64":
		d.b64 = str(src, "data")
	case "url":
		d.url = str(src, "url")
	case "file":
		d.fileID = str(src, "file_id")
	case "content":
		var s string
		if json.Unmarshal(src["content"], &s) == nil {
			d.text, d.hasText = s, true
			break
		}
		blocks, _ := array(src["content"])
		var texts []string
		for _, blk := range blocks {
			bm, ok := object(blk)
			if !ok {
				continue
			}
			switch str(bm, "type") {
			case "text":
				texts = append(texts, str(bm, "text"))
			case "image":
				d.images = append(d.images, blk)
			default:
				texts = append(texts, omitted(str(bm, "type")))
			}
		}
		if len(texts) > 0 {
			d.text, d.hasText = strings.Join(texts, "\n\n"), true
		}
	}
	return d
}

// claudeDocumentPieces renders a document block. Images from a
// content-source document stay images where the position allows them
// (user content, tool results) and become markers elsewhere.
func claudeDocumentPieces(m map[string]json.RawMessage, allowImages bool) []piece {
	d := claudeDocument(m)
	text := d.render()
	pieces := []piece{synthText(text)}
	for _, img := range d.images {
		if allowImages && imageUsable(img) {
			pieces = append(pieces, keep(img))
		} else {
			pieces = append(pieces, synthText(imageMarker(img)))
		}
	}
	return pieces
}

// responsesFile reads a Responses input_file part.
func responsesFile(m map[string]json.RawMessage) doc {
	d := doc{title: str(m, "filename")}
	switch data := str(m, "file_data"); {
	case strings.HasPrefix(data, "data:"):
		meta, payload, _ := strings.Cut(strings.TrimPrefix(data, "data:"), ",")
		d.mediaType = strings.TrimSuffix(meta, ";base64")
		if strings.HasSuffix(meta, ";base64") {
			d.b64 = payload
		} else {
			d.text, d.hasText = payload, true
		}
	case data != "":
		d.mediaType = mediaTypeFromName(d.title)
		d.b64 = data
	case str(m, "file_url") != "":
		d.url = str(m, "file_url")
	default:
		d.fileID = str(m, "file_id")
	}
	return d
}
