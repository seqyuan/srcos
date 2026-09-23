package web

import (
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// This is the Markdown viewer's engine. It is a deliberately small subset of
// CommonMark + GFM: enough for the READMEs, notes and reports a lab actually
// keeps next to its data, and small enough to audit.
//
// **Safety**: the input is HTML-escaped first, and every tag emitted below is
// written by this file. Nothing from the document can introduce markup, an
// event handler or a `javascript:` URL, so the output needs no separate
// sanitizer (which would be a second parser to get wrong).
//
// Supported blocks: ATX headings, fenced and indented code, block quotes,
// unordered/ordered lists (one level), GFM pipe tables, thematic breaks,
// paragraphs. Supported inline: code spans, bold (`**`/`__`), italic (`*`),
// strikethrough (`~~`), links and images with an absolute http(s)/mailto URL,
// autolinks.
//
// Not supported, on purpose: nested lists, reference links, raw HTML, setext
// headings, and `_italic_` (a single underscore is too easy to meet inside an
// identifier like `total_count`, and mangling those is worse than missing the
// emphasis).
func renderMarkdown(src string) string {
	src = strings.ReplaceAll(src, "\r\n", "\n")
	src = strings.ReplaceAll(src, "\r", "\n")
	lines := strings.Split(src, "\n")

	var out strings.Builder
	for i := 0; i < len(lines); {
		line := lines[i]

		// Blank line: nothing to emit, and the surrounding blocks have already
		// been closed.
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}

		if fence, ok := fenceInfo(line); ok {
			body, next := collectFence(lines, i+1, fence)
			out.WriteString(renderCodeBlock(body, fence.lang))
			i = next
			continue
		}

		if lvl, text, ok := atxHeading(line); ok {
			fmt.Fprintf(&out, "<h%d>%s</h%d>\n", lvl, renderInline(text), lvl)
			i++
			continue
		}

		if isThematicBreak(line) {
			out.WriteString("<hr>\n")
			i++
			continue
		}

		if isIndentedCode(line) {
			body, next := collectIndentedCode(lines, i)
			out.WriteString(renderCodeBlock(body, ""))
			i = next
			continue
		}

		if isBlockQuote(line) {
			body, next := collectBlockQuote(lines, i)
			out.WriteString("<blockquote>\n" + renderMarkdown(body) + "</blockquote>\n")
			i = next
			continue
		}

		if items, next, ok := collectList(lines, i); ok {
			out.WriteString(renderList(items))
			i = next
			continue
		}

		if table, next, ok := collectTable(lines, i); ok {
			out.WriteString(renderTable(table))
			i = next
			continue
		}

		// Paragraph: consume until a blank line or the start of another block.
		var para []string
		for i < len(lines) {
			l := lines[i]
			if strings.TrimSpace(l) == "" {
				break
			}
			if _, ok := fenceInfo(l); ok {
				break
			}
			if _, _, ok := atxHeading(l); ok {
				break
			}
			if isThematicBreak(l) {
				break
			}
			if isBlockQuote(l) || isListLine(l) || isIndentedCode(l) {
				break
			}
			if _, ok := tableSep(l, nextLine(lines, i+1)); ok && len(para) == 0 {
				break
			}
			para = append(para, strings.TrimSpace(l))
			i++
		}
		if len(para) > 0 {
			out.WriteString("<p>" + renderInline(strings.Join(para, " ")) + "</p>\n")
		} else {
			// Defensive: the loop above always advances for a non-blank line,
			// but a guard keeps a parser bug from becoming an infinite loop.
			i++
		}
	}
	return out.String()
}

func nextLine(lines []string, i int) string {
	if i >= 0 && i < len(lines) {
		return lines[i]
	}
	return ""
}

// ── blocks ───────────────────────────────────────────────────────────────

type fence struct {
	lang string
}

// fenceInfo recognises an opening code fence (``` or ~~~, three or more).
func fenceInfo(line string) (fence, bool) {
	t := strings.TrimSpace(line)
	if len(t) < 3 {
		return fence{}, false
	}
	ch := t[0]
	if ch != '`' && ch != '~' {
		return fence{}, false
	}
	n := 0
	for n < len(t) && t[n] == ch {
		n++
	}
	if n < 3 {
		return fence{}, false
	}
	info := strings.TrimSpace(t[n:])
	if ch == '`' && strings.Contains(info, "`") {
		return fence{}, false
	}
	// The info string's first word is the language.
	if i := strings.IndexAny(info, " \t"); i >= 0 {
		info = info[:i]
	}
	return fence{lang: info}, true
}

// collectFence gathers a fenced block's body, ending at a closing fence of the
// same character (or at the end of input, which CommonMark also accepts).
func collectFence(lines []string, start int, f fence) (string, int) {
	var body []string
	i := start
	for ; i < len(lines); i++ {
		if close, ok := fenceInfo(lines[i]); ok && close.lang == "" {
			if t := strings.TrimSpace(lines[i]); len(t) >= 3 {
				i++
				break
			}
		}
		body = append(body, lines[i])
	}
	return strings.Join(body, "\n"), i
}

func renderCodeBlock(body, lang string) string {
	class := ""
	if lang != "" {
		class = ` class="language-` + html.EscapeString(lang) + `"`
	}
	return "<pre><code" + class + ">" + html.EscapeString(body) + "</code></pre>\n"
}

// atxHeading recognises `# heading`.
func atxHeading(line string) (int, string, bool) {
	t := strings.TrimLeft(line, " \t")
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return 0, "", false
	}
	if n < len(t) && t[n] != ' ' && t[n] != '\t' {
		return 0, "", false
	}
	return n, strings.TrimSpace(t[n:]), true
}

// isThematicBreak recognises `---`, `***`, `___` (three or more, spaces
// allowed between them).
func isThematicBreak(line string) bool {
	t := strings.TrimSpace(line)
	if len(t) < 3 {
		return false
	}
	ch := t[0]
	if ch != '-' && ch != '*' && ch != '_' {
		return false
	}
	n := 0
	for i := 0; i < len(t); i++ {
		switch t[i] {
		case ch:
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return n >= 3
}

func isIndentedCode(line string) bool {
	return strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
}

func collectIndentedCode(lines []string, start int) (string, int) {
	var body []string
	i := start
	for ; i < len(lines); i++ {
		switch {
		case isIndentedCode(lines[i]):
			body = append(body, strings.TrimPrefix(strings.TrimPrefix(lines[i], "\t"), "    "))
		case strings.TrimSpace(lines[i]) == "":
			body = append(body, "")
		default:
			return strings.TrimRight(strings.Join(body, "\n"), "\n"), i
		}
	}
	return strings.TrimRight(strings.Join(body, "\n"), "\n"), i
}

func isBlockQuote(line string) bool {
	t := strings.TrimLeft(line, " \t")
	return strings.HasPrefix(t, ">")
}

func collectBlockQuote(lines []string, start int) (string, int) {
	var body []string
	i := start
	for ; i < len(lines); i++ {
		t := strings.TrimLeft(lines[i], " \t")
		if !strings.HasPrefix(t, ">") {
			break
		}
		t = strings.TrimPrefix(t, ">")
		t = strings.TrimPrefix(t, " ")
		body = append(body, t)
	}
	return strings.Join(body, "\n"), i
}

// listItem is one collected list item with its raw text.
type listItem struct {
	text      string
	ordered   bool
	number    int
	continues []string
}

func isListLine(line string) bool {
	t := strings.TrimLeft(line, " \t")
	if strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "+ ") {
		return true
	}
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	return i > 0 && i+1 < len(t) && t[i] == '.' && t[i+1] == ' '
}

// collectList gathers a run of list items: the items, the index after the
// list, and whether it found any.
func collectList(lines []string, start int) ([]listItem, int, bool) {
	if !isListLine(lines[start]) {
		return nil, start, false
	}
	var items []listItem
	i := start
	for i < len(lines) {
		if !isListLine(lines[i]) {
			// A blank line followed by another item continues the list.
			if strings.TrimSpace(lines[i]) == "" {
				j := i
				for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
					j++
				}
				if j < len(lines) && isListLine(lines[j]) {
					i = j
					continue
				}
			}
			break
		}
		t := strings.TrimLeft(lines[i], " \t")
		item := listItem{}
		switch {
		case strings.HasPrefix(t, "- "), strings.HasPrefix(t, "* "), strings.HasPrefix(t, "+ "):
			item.text = strings.TrimSpace(t[2:])
		default:
			j := 0
			for j < len(t) && t[j] >= '0' && t[j] <= '9' {
				j++
			}
			item.ordered = true
			item.number, _ = strconv.Atoi(t[:j])
			item.text = strings.TrimSpace(t[j+1:])
		}
		i++
		// A following indented non-item line is a lazy continuation.
		for i < len(lines) && strings.TrimSpace(lines[i]) != "" && !isListLine(lines[i]) && isIndentedCode(lines[i]) {
			item.continues = append(item.continues, strings.TrimSpace(lines[i]))
			i++
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, start, false
	}
	return items, i, true
}

func renderList(items []listItem) string {
	ordered := items[0].ordered
	tag := "ul"
	if ordered {
		tag = "ol"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<%s>\n", tag)
	for _, it := range items {
		text := it.text
		if len(it.continues) > 0 {
			text += " " + strings.Join(it.continues, " ")
		}
		// A checkbox is the one GFM extra worth the few lines.
		class := ""
		if strings.HasPrefix(text, "[ ] ") {
			class = ` class="task"`
			text = `<input type="checkbox" disabled> ` + html.EscapeString(strings.TrimPrefix(text, "[ ] "))
			b.WriteString("<li" + class + ">" + text + "</li>\n")
			continue
		}
		if strings.HasPrefix(strings.ToLower(text), "[x] ") {
			class = ` class="task"`
			text = `<input type="checkbox" disabled checked> ` + html.EscapeString(text[4:])
			b.WriteString("<li" + class + ">" + text + "</li>\n")
			continue
		}
		b.WriteString("<li>" + renderInline(text) + "</li>\n")
	}
	fmt.Fprintf(&b, "</%s>\n", tag)
	return b.String()
}

// ── tables ───────────────────────────────────────────────────────────────

type tableCell struct {
	text  string
	align string
}

type tableRow []tableCell

func splitTableRow(line string) []string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	t = strings.TrimSuffix(t, "|")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// tableSep recognises the `|---|---|` separator row and returns the alignment of
// each column.
func tableSep(header, sep string) ([]string, bool) {
	if !strings.Contains(header, "|") || !strings.Contains(sep, "|") {
		return nil, false
	}
	cells := splitTableRow(sep)
	if len(cells) == 0 {
		return nil, false
	}
	aligns := make([]string, 0, len(cells))
	for _, c := range cells {
		c = strings.TrimSpace(c)
		if c == "" {
			return nil, false
		}
		left := strings.HasPrefix(c, ":")
		right := strings.HasSuffix(c, ":")
		body := strings.Trim(c, ":")
		if len(body) < 1 || strings.Trim(body, "-") != "" {
			return nil, false
		}
		switch {
		case left && right:
			aligns = append(aligns, "center")
		case right:
			aligns = append(aligns, "right")
		case left:
			aligns = append(aligns, "left")
		default:
			aligns = append(aligns, "")
		}
	}
	return aligns, true
}

func collectTable(lines []string, start int) ([][]tableCell, int, bool) {
	aligns, ok := tableSep(nextLine(lines, start), nextLine(lines, start+1))
	if !ok {
		return nil, start, false
	}
	header := splitTableRow(lines[start])
	if len(header) != len(aligns) {
		return nil, start, false
	}
	var rows [][]tableCell
	rows = append(rows, rowOf(header, aligns))
	i := start + 2
	for i < len(lines) {
		if strings.TrimSpace(lines[i]) == "" || !strings.Contains(lines[i], "|") {
			break
		}
		cells := splitTableRow(lines[i])
		rows = append(rows, rowOf(cells, aligns))
		i++
	}
	return rows, i, true
}

func rowOf(cells []string, aligns []string) []tableCell {
	row := make([]tableCell, 0, len(cells))
	for i, c := range cells {
		align := ""
		if i < len(aligns) {
			align = aligns[i]
		}
		row = append(row, tableCell{text: c, align: align})
	}
	return row
}

func renderTable(rows [][]tableCell) string {
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<table class="md-table">` + "\n" + "<thead><tr>")
	for _, c := range rows[0] {
		fmt.Fprintf(&b, "<th%s>%s</th>", alignAttr(c.align), renderInline(c.text))
	}
	b.WriteString("</tr></thead>\n<tbody>\n")
	for _, row := range rows[1:] {
		b.WriteString("<tr>")
		for _, c := range row {
			fmt.Fprintf(&b, "<td%s>%s</td>", alignAttr(c.align), renderInline(c.text))
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("</tbody></table>\n")
	return b.String()
}

func alignAttr(a string) string {
	if a == "" {
		return ""
	}
	return ` class="align-` + a + `"`
}

// ── inline ───────────────────────────────────────────────────────────────

var (
	codeSpanRe        = regexp.MustCompile("`([^`]+)`")
	codePlaceholderRe = regexp.MustCompile("\x01[0-9]+\x01")
	imageRe           = regexp.MustCompile(`!\[([^\]]*)\]\(([^)\s]*)\)`)
	linkRe            = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]*)\)`)
	autolinkRe        = regexp.MustCompile(`&lt;((?:https?|mailto):[^&\s<>]+)&gt;`)
	boldRe            = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	boldURe           = regexp.MustCompile(`__([^_]+)__`)
	italicRe          = regexp.MustCompile(`\*([^*]+)\*`)
	strikeRe          = regexp.MustCompile(`~~([^~]+)~~`)
)

// renderInline applies inline formatting to already-escaped text.
func renderInline(text string) string {
	text = html.EscapeString(text)

	// Pull code spans out first: their contents must not be formatted, and a
	// placeholder is cheaper than a stateful scanner.
	var spans []string
	text = codeSpanRe.ReplaceAllStringFunc(text, func(m string) string {
		spans = append(spans, "<code>"+m[1:len(m)-1]+"</code>")
		return fmt.Sprintf("\x01%d\x01", len(spans)-1)
	})

	text = imageRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := imageRe.FindStringSubmatch(m)
		url, ok := safeURL(sub[2])
		if !ok {
			return sub[0] // no URL we are willing to load: leave the source visible
		}
		return `<img src="` + url + `" alt="` + sub[1] + `" loading="lazy">`
	})
	text = linkRe.ReplaceAllStringFunc(text, func(m string) string {
		sub := linkRe.FindStringSubmatch(m)
		url, ok := safeURL(sub[2])
		if !ok {
			return sub[0]
		}
		label := sub[1]
		if label == "" {
			label = url
		}
		return `<a href="` + url + `" target="_blank" rel="noopener noreferrer">` + label + `</a>`
	})
	text = autolinkRe.ReplaceAllString(text, `<a href="$1" target="_blank" rel="noopener noreferrer">$1</a>`)
	text = boldRe.ReplaceAllString(text, `<strong>$1</strong>`)
	text = boldURe.ReplaceAllString(text, `<strong>$1</strong>`)
	text = strikeRe.ReplaceAllString(text, `<del>$1</del>`)
	text = italicRe.ReplaceAllString(text, `<em>$1</em>`)

	// Restore the code spans.
	text = codePlaceholderRe.ReplaceAllStringFunc(text, func(m string) string {
		idx, err := strconv.Atoi(strings.Trim(m, "\x01"))
		if err != nil || idx < 0 || idx >= len(spans) {
			return ""
		}
		return spans[idx]
	})
	return text
}

// safeURL accepts only absolute http(s)/mailto URLs and in-page anchors.
//
// A relative link is refused on purpose: it would resolve against /view rather
// than against the file being previewed, so it would silently point somewhere
// else — better to leave the Markdown source visible than to offer a link that
// goes to the wrong place.
func safeURL(raw string) (string, bool) {
	u := strings.TrimSpace(raw)
	if u == "" {
		return "", false
	}
	if strings.HasPrefix(u, "#") {
		return u, true
	}
	// Compare schemes with whitespace and control characters removed, so
	// "java\nscript:" cannot slip past the check.
	compact := strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToLower(u))
	for _, bad := range []string{"javascript:", "vbscript:", "data:"} {
		if strings.HasPrefix(compact, bad) {
			return "", false
		}
	}
	for _, ok := range []string{"http://", "https://", "mailto:"} {
		if strings.HasPrefix(compact, ok) {
			return u, true
		}
	}
	return "", false
}
