package web

import (
	"encoding/csv"
	"fmt"
	"net/url"
	"strings"

	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/resource"
)

// resourceCSS styles the viewer. It is injected into the page body rather than
// base.css because the viewer is one page, and keeping its rules beside the
// markup that uses them keeps the two in step.
const resourceCSS = `<style>
.rv-head { margin-bottom: 14px; }
.rv-head h1 { margin: 0 0 4px; font-size: 20px; }
.rv-meta { font-size: 13px; word-break: break-all; }
.rv-actions { margin-top: 10px; display: flex; flex-wrap: wrap; gap: 8px 14px; align-items: center; font-size: 13px; }
.rv-actions a { color: var(--accent); text-decoration: none; }
.rv-actions a:hover { text-decoration: underline; }
.rv-switch { color: var(--text-muted); }
.rv-switch a { color: var(--accent); text-decoration: none; }
.rv-body { background: var(--bg-panel); border: 1px solid var(--border); border-radius: var(--r-card); overflow: hidden; }
.rv-note { padding: 10px 16px; margin: 0; font-size: 13px; }
.rv-code { overflow: auto; max-height: 74vh; font-family: var(--mono); font-size: 12.5px; line-height: 1.55; padding: 12px 0; }
.rv-line { display: flex; white-space: pre; }
.rv-line:hover { background: var(--hover); }
.rv-ln { flex: 0 0 auto; width: 5ch; text-align: right; padding-right: 14px; color: var(--text-muted); user-select: none; opacity: .7; }
.rv-txt { flex: 1 1 auto; padding-right: 16px; }
.rv-dir, .rv-table { width: 100%; border-collapse: collapse; font-size: 13.5px; }
.rv-dir th, .rv-dir td, .rv-table th, .rv-table td { padding: 8px 14px; border-bottom: 1px solid var(--border); text-align: left; }
.rv-dir th, .rv-table th { background: var(--bg); font-weight: 600; position: sticky; top: 0; }
.rv-dir a, .rv-table a { color: var(--accent); text-decoration: none; }
.rv-dir a:hover { text-decoration: underline; }
.rv-dir td.num, .rv-table td.num { text-align: right; }
.rv-dir td.mono { font-family: var(--mono); font-size: 12.5px; color: var(--text-muted); }
.rv-tablewrap { overflow: auto; max-height: 76vh; }
.rv-img { display: block; max-width: 100%; max-height: 78vh; margin: 0 auto; }
.rv-frame { display: block; width: 100%; height: 78vh; border: 0; background: #fff; }
.md { padding: 18px 22px 26px; line-height: 1.72; font-size: 14.5px; }
.md h1, .md h2, .md h3, .md h4 { margin: 1.3em 0 .5em; line-height: 1.3; }
.md h1 { font-size: 22px; } .md h2 { font-size: 19px; } .md h3 { font-size: 16px; } .md h4 { font-size: 15px; }
.md p { margin: .7em 0; }
.md code { font-family: var(--mono); font-size: 12.5px; background: var(--bg); padding: 1px 5px; border-radius: 5px; }
.md pre { background: var(--bg); border: 1px solid var(--border); border-radius: var(--r-chip); padding: 12px 14px; overflow: auto; }
.md pre code { background: none; padding: 0; }
.md blockquote { margin: .8em 0; padding: 2px 16px; border-left: 3px solid var(--border); color: var(--text-muted); }
.md table { border-collapse: collapse; margin: .8em 0; font-size: 13.5px; }
.md th, .md td { border: 1px solid var(--border); padding: 6px 12px; text-align: left; }
.md th { background: var(--bg); }
.md .align-right { text-align: right; } .md .align-center { text-align: center; }
.md img { max-width: 100%; }
.md hr { border: 0; border-top: 1px solid var(--border); margin: 1.4em 0; }
.md ul, .md ol { padding-left: 1.6em; margin: .6em 0; }
.md li.task { list-style: none; margin-left: -1.2em; }
.rv-scopes { display: grid; gap: 22px; }
.rv-group { font-size: 14px; color: var(--text-muted); text-transform: uppercase; letter-spacing: .04em; margin: 6px 0 0; }
.rv-scope-list { list-style: none; padding: 0; margin: 0; display: grid; gap: 8px; }
.rv-scope-list li { background: var(--bg-panel); border: 1px solid var(--border); border-radius: var(--r-chip); padding: 10px 14px; }
.rv-scope-list a { color: var(--accent); text-decoration: none; font-weight: 600; }
.rv-scope-list a:hover { text-decoration: underline; }
.rv-scope-src { font-family: var(--mono); font-size: 12px; color: var(--text-muted); margin-top: 3px; }
.rv-mode { font-size: 11px; color: var(--text-muted); border: 1px solid var(--border); border-radius: var(--r-pill); padding: 1px 8px; margin-left: 6px; }
</style>`

// This file renders the resource viewer (roadmap Phase 5). It is the platform's
// own viewer — which must work with no external runtime, no Node and no build
// step. Rendering is server-side and every page is a plain Go
// template, exactly like the tool pages (ADR-012 keeps React for the canvas
// alone).
//
// The one safety-critical rule here is ADR-011's: user HTML is never rendered
// in the gateway's origin. It goes into a `sandbox` iframe (no
// allow-same-origin, so the document lands in an opaque origin and cannot touch
// the gateway's cookies or its same-origin management API), and the response
// served into that frame carries a matching CSP sandbox as a second lock.

// ResourcePageData is everything the viewer page renders from.
type ResourcePageData struct {
	SiteTitle string
	// Src is the canonical address; empty never reaches here (the chooser page
	// handles it).
	Src string
	// Addr is the parsed address, for building breadcrumbs and child links.
	Addr resource.Address
	// View is the read side's answer (metadata + bounded payload).
	View *inspect.ResourceView
	// ScopeName is a human title for the scope (e.g. the storage's name).
	ScopeName string
	// Viewers lists the registry in claim order for the "open as" switcher.
	Viewers []resource.Viewer
}

// ResourcePage renders one resource.
func ResourcePage(d ResourcePageData) string {
	v := d.View
	if v == nil {
		return PageShell(d.SiteTitle, "资源", `<main class="wrap"><p class="muted">资源不可用。</p></main>`)
	}
	var b strings.Builder
	b.WriteString(resourceCSS)
	b.WriteString(`<main class="wrap rv">`)

	// Breadcrumb + actions.
	b.WriteString(`<p class="crumb">`)
	b.WriteString(`<a href="/view">资源</a>`)
	for _, crumb := range breadcrumbs(d.Addr) {
		b.WriteString(` / `)
		if crumb.current {
			b.WriteString(esc(crumb.label))
		} else {
			fmt.Fprintf(&b, `<a href="/view?src=%s">%s</a>`, url.QueryEscape(crumb.src), esc(crumb.label))
		}
	}
	b.WriteString(`</p>`)

	// Headline: the file's base name, or the scope's name at the root.
	name := baseName(v.SandboxPath)
	if d.Addr.Path == "" {
		name = d.ScopeName
	}
	fmt.Fprintf(&b, `<header class="rv-head"><h1>%s</h1>`, esc(name))
	fmt.Fprintf(&b, `<div class="muted rv-meta">%s · %s%s%s · 视图 %s</div>`,
		esc(v.SandboxPath),
		esc(humanBytes(v.Size)),
		ternary(v.ModTime.IsZero(), "", " · "+v.ModTime.Format("2006-01-02 15:04")),
		ternary(v.Mode == "", "", " · "+v.Mode),
		esc(v.Viewer.Name),
	)

	fmt.Fprintf(&b, `<div class="rv-actions"><a href="%s">原始文件</a>`,
		esc("/api/resources/raw?src="+url.QueryEscape(d.Src)))
	if v.Viewer.Kind == resource.KindHTML {
		fmt.Fprintf(&b, `<a href="%s" target="_blank" rel="noopener noreferrer">新窗口打开（sandbox）</a>`,
			esc("/api/resources/html?src="+url.QueryEscape(d.Src)))
	}
	if parent, ok := d.Addr.Parent(); ok {
		fmt.Fprintf(&b, `<a href="/view?src=%s">上一级</a>`, url.QueryEscape(parent.String()))
	}
	b.WriteString(viewerSwitcher(d))
	b.WriteString(`</div></header>`)

	b.WriteString(`<section class="rv-body">`)
	switch {
	case v.IsDir:
		b.WriteString(renderDirBody(d))
	case v.Binary:
		b.WriteString(`<p class="muted">这是二进制文件，无法以文本预览。可以<a href="` +
			esc("/api/resources/raw?src="+url.QueryEscape(d.Src)) + `">下载原始文件</a>。</p>`)
	default:
		switch v.Viewer.Kind {
		case resource.KindText:
			b.WriteString(renderTextBody(v))
		case resource.KindMarkdown:
			b.WriteString(`<article class="md">` + renderMarkdown(v.Text) + `</article>`)
			b.WriteString(truncNote(v.Truncated))
		case resource.KindTable:
			b.WriteString(renderTableBody(v.Text, delimFor(d.Addr.Path)))
			b.WriteString(truncNote(v.Truncated))
		case resource.KindImage:
			fmt.Fprintf(&b, `<img class="rv-img" alt="%s" src="%s">`,
				esc(baseName(v.SandboxPath)),
				esc("/api/resources/raw?src="+url.QueryEscape(d.Src)))
		case resource.KindPDF:
			fmt.Fprintf(&b, `<iframe class="rv-frame rv-pdf" title="PDF" src="%s"></iframe>`,
				esc("/api/resources/raw?src="+url.QueryEscape(d.Src)))
		case resource.KindHTML:
			// ADR-011: sandbox iframe + an opaque origin. Deliberately no
			// `allow-same-origin`, so the document cannot reach the gateway's
			// session cookie or its same-origin management API.
			fmt.Fprintf(&b, `<iframe class="rv-frame rv-html" title="HTML 预览" sandbox="allow-scripts allow-forms allow-popups allow-modals allow-downloads" src="%s"></iframe>`,
				esc("/api/resources/html?src="+url.QueryEscape(d.Src)))
			b.WriteString(`<p class="muted rv-note">HTML 在 sandbox iframe 中渲染（独立 origin、无同源权限），
页内脚本无法访问网关的会话或管理 API。</p>`)
		default:
			b.WriteString(`<p class="muted">没有可用的预览方式，请下载原始文件。</p>`)
		}
	}
	b.WriteString(`</section></main>`)

	return PageShell(d.SiteTitle, name, b.String())
}

// ResourceErrorPage explains why an address could not be opened.
func ResourceErrorPage(siteTitle, msg string) string {
	return NoticePage(siteTitle, "打不开这个资源", msg, "/view", "返回资源浏览")
}

// ResourceScopesPage is the landing page: the roots this user may browse, so
// the viewer is reachable without knowing an address by heart.
func ResourceScopesPage(siteTitle string, scopes []inspect.ResourceScope) string {
	var b strings.Builder
	b.WriteString(resourceCSS)
	b.WriteString(`<main class="wrap rv"><p class="crumb"><a href="/">仪表盘</a> / 资源</p>`)
	b.WriteString(`<h1>资源</h1>`)
	b.WriteString(`<p class="muted">浏览你的 home、各工具的工作区，以及已授权的共享数据。
地址协议为 <code>srcos://file/&lt;scope&gt;/&lt;path&gt;</code>，与 <code>dsh-resource://</code> 同构。</p>`)

	if len(scopes) == 0 {
		b.WriteString(`<p class="muted">没有可浏览的位置。</p></main>`)
		return PageShell(siteTitle, "资源", b.String())
	}

	var home, workspaces, storages []inspect.ResourceScope
	for _, s := range scopes {
		switch s.Kind {
		case "workspace":
			workspaces = append(workspaces, s)
		case "storage":
			storages = append(storages, s)
		default:
			home = append(home, s)
		}
	}
	b.WriteString(`<div class="rv-scopes">`)
	writeScopeGroup(&b, "我的", home)
	writeScopeGroup(&b, "工作区", workspaces)
	writeScopeGroup(&b, "共享数据", storages)
	b.WriteString(`</div>`)

	if len(storages) == 0 {
		b.WriteString(`<p class="muted rv-note">还没有任何工具声明 <code>requires_storages</code>，因此没有共享数据可选
（ADR-020：可选范围 == 已挂载 storage == 工具声明）。</p>`)
	}
	b.WriteString(`</main>`)
	return PageShell(siteTitle, "资源", b.String())
}

func writeScopeGroup(b *strings.Builder, title string, scopes []inspect.ResourceScope) {
	if len(scopes) == 0 {
		return
	}
	fmt.Fprintf(b, `<h2 class="rv-group">%s</h2><ul class="rv-scope-list">`, esc(title))
	for _, s := range scopes {
		addr := resource.Address{Provider: resource.ProviderFile, Scope: s.Scope, Tool: s.Tool}
		mode := ""
		if s.Mode != "" {
			mode = ` <span class="rv-mode">` + esc(s.Mode) + `</span>`
		}
		fmt.Fprintf(b, `<li><a href="/view?src=%s">%s</a>%s<div class="rv-scope-src">%s</div></li>`,
			url.QueryEscape(addr.String()), esc(s.Name), mode, esc(addr.String()))
	}
	b.WriteString(`</ul>`)
}

// ── bodies ───────────────────────────────────────────────────────────────

// maxTextLines bounds the line-numbered text viewer. A 10-million-line log is a
// download, not a page.
const maxTextLines = 5000

func renderTextBody(v *inspect.ResourceView) string {
	lines := strings.Split(v.Text, "\n")
	// A trailing newline makes Split produce a final empty element; showing it
	// as a blank line 5001 would be noise.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	dropped := 0
	if len(lines) > maxTextLines {
		dropped = len(lines) - maxTextLines
		lines = lines[:maxTextLines]
	}
	var b strings.Builder
	b.WriteString(`<div class="rv-code">`)
	width := len(fmt.Sprintf("%d", len(lines)+dropped))
	for i, line := range lines {
		fmt.Fprintf(&b, `<div class="rv-line"><span class="rv-ln">%*d</span><span class="rv-txt">%s</span></div>`,
			width, i+1, esc(line))
	}
	b.WriteString(`</div>`)
	if dropped > 0 {
		fmt.Fprintf(&b, `<p class="muted rv-note">还有 %d 行未显示（上限 %d 行）。</p>`, dropped, maxTextLines)
	}
	return b.String()
}

func renderDirBody(d ResourcePageData) string {
	v := d.View
	if len(v.Entries) == 0 {
		return `<p class="muted">空目录。</p>`
	}
	// The parent row keeps ".. navigation" possible without a breadcrumb click.
	var b strings.Builder
	b.WriteString(`<table class="rv-dir"><thead><tr><th>名称</th><th class="num">大小</th><th>修改时间</th><th>视图</th></tr></thead><tbody>`)
	if parent, ok := d.Addr.Parent(); ok {
		fmt.Fprintf(&b, `<tr><td><a href="/view?src=%s">..</a></td><td></td><td></td><td></td></tr>`,
			url.QueryEscape(parent.String()))
	}
	registry := resource.Default()
	for _, e := range v.Entries {
		// The entry's Rel is relative to the *scope*, so it replaces the address's
		// path rather than joining onto it — joining would double the directory
		// (a bug the browser e2e caught, not the unit tests).
		child := d.Addr
		child.Path = e.Rel
		icon := "·"
		if e.IsDir {
			icon = "📁"
		}
		viewerName := ""
		if !e.IsDir {
			viewerName = registry.Claim(e.Name, false).Name
		}
		size := ""
		if !e.IsDir {
			size = humanBytes(e.Size)
		}
		mtime := ""
		if !e.ModTime.IsZero() {
			mtime = e.ModTime.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(&b, `<tr><td>%s <a href="/view?src=%s">%s</a></td><td class="num">%s</td><td class="mono">%s</td><td class="muted">%s</td></tr>`,
			icon, url.QueryEscape(child.String()), esc(e.Name), esc(size), esc(mtime), esc(viewerName))
	}
	b.WriteString(`</tbody></table>`)
	if v.EntriesTruncated {
		fmt.Fprintf(&b, `<p class="muted rv-note">目录条目过多，只显示前 %d 项。</p>`, len(v.Entries))
	}
	return b.String()
}

// TableData is a parsed CSV/TSV preview.
type TableData struct {
	Header    []string
	Rows      [][]string
	Delimiter string
	// TruncatedRows/Cols report that the file had more than the preview shows.
	TruncatedRows bool
	TruncatedCols bool
	TotalRows     int
	TotalCols     int
}

// Table preview caps. A preview that can never finish rendering is not a
// preview; the raw download is one click away.
const (
	maxTableRows = 500
	maxTableCols = 40
)

// ParseTable reads delimited text into a preview, tolerating ragged rows (a
// hand-edited CSV is a normal thing to find next to data).
//
// It never fails: unparseable input yields what could be read, because a viewer
// that refuses to show a slightly malformed file is less useful than one that
// shows the part it understood.
func ParseTable(text string, delim rune) *TableData {
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true

	records, _ := r.ReadAll()
	td := &TableData{Delimiter: string(delim)}
	if len(records) == 0 {
		return td
	}
	td.Header = records[0]
	body := records[1:]
	td.TotalRows = len(body)

	maxCols := len(td.Header)
	for _, row := range body {
		if len(row) > maxCols {
			maxCols = len(row)
		}
	}
	td.TotalCols = maxCols
	if maxCols > maxTableCols {
		td.TruncatedCols = true
	}
	if len(body) > maxTableRows {
		td.TruncatedRows = true
		body = body[:maxTableRows]
	}
	trim := func(row []string) []string {
		if len(row) > maxTableCols {
			return row[:maxTableCols]
		}
		return row
	}
	td.Header = trim(td.Header)
	for _, row := range body {
		td.Rows = append(td.Rows, trim(row))
	}
	return td
}

// delimFor picks the field delimiter from the file name, falling back to a
// content sniff so a `.txt` export is still readable.
func delimFor(path string) rune {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".tsv"), strings.HasSuffix(lower, ".tab"):
		return '\t'
	default:
		return ','
	}
}

// sniffDelim keeps a `.txt` (or extension-less) export readable: when the
// declared delimiter does not occur in the first line but the other one does,
// the content wins over the name.
func sniffDelim(text string, delim rune) rune {
	line := text
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if strings.ContainsRune(line, delim) {
		return delim
	}
	switch delim {
	case ',':
		if strings.ContainsRune(line, '\t') {
			return '\t'
		}
	case '\t':
		if strings.ContainsRune(line, ',') {
			return ','
		}
	}
	return delim
}

func renderTableBody(text string, delim rune) string {
	delim = sniffDelim(text, delim)
	td := ParseTable(text, delim)
	if len(td.Header) == 0 {
		return `<p class="muted">表格为空。</p>`
	}
	var b strings.Builder
	b.WriteString(`<div class="rv-tablewrap"><table class="rv-table"><thead><tr>`)
	for _, h := range td.Header {
		fmt.Fprintf(&b, `<th>%s</th>`, esc(h))
	}
	b.WriteString(`</tr></thead><tbody>`)
	width := len(td.Header)
	for _, row := range td.Rows {
		b.WriteString(`<tr>`)
		for i := 0; i < width; i++ {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			fmt.Fprintf(&b, `<td>%s</td>`, esc(cell))
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></div>`)
	if td.TruncatedRows || td.TruncatedCols {
		fmt.Fprintf(&b, `<p class="muted rv-note">只显示前 %d 行 × %d 列（原表约 %d 行 × %d 列）。</p>`,
			len(td.Rows), width, td.TotalRows, td.TotalCols)
	}
	return b.String()
}

// ── helpers ──────────────────────────────────────────────────────────────

func viewerSwitcher(d ResourcePageData) string {
	if len(d.Viewers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<span class="rv-switch">打开为：`)
	first := true
	for _, v := range d.Viewers {
		if v.Kind == resource.KindDir {
			continue
		}
		if !first {
			b.WriteString(` · `)
		}
		first = false
		href := "/view?src=" + url.QueryEscape(d.Src) + "&viewer=" + url.QueryEscape(v.ID)
		if v.ID == d.View.Viewer.ID {
			fmt.Fprintf(&b, `<strong>%s</strong>`, esc(v.Name))
		} else {
			fmt.Fprintf(&b, `<a href="%s">%s</a>`, esc(href), esc(v.Name))
		}
	}
	b.WriteString(`</span>`)
	return b.String()
}

type breadcrumb struct {
	label   string
	src     string
	current bool
}

// breadcrumbs builds the trail from the scope root to the current entry.
func breadcrumbs(a resource.Address) []breadcrumb {
	root := a
	root.Path = ""
	out := []breadcrumb{{label: a.Scope, src: root.String()}}
	segs := []string{}
	if a.Path != "" {
		segs = strings.Split(a.Path, "/")
	}
	for i, seg := range segs {
		cur := a
		cur.Path = strings.Join(segs[:i+1], "/")
		out = append(out, breadcrumb{label: seg, src: cur.String(), current: i == len(segs)-1})
	}
	if len(segs) == 0 {
		out[0].current = true
	}
	return out
}

func truncNote(truncated bool) string {
	if !truncated {
		return ""
	}
	return `<p class="muted rv-note">文件较大，只预览了前面一部分。</p>`
}

func baseName(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// humanBytes renders a byte count for a header line.
func humanBytes(n int64) string {
	const (
		kb = int64(1) << 10
		mb = int64(1) << 20
		gb = int64(1) << 30
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
