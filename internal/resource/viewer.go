package resource

import (
	"sort"
	"strings"
)

// Kind is how a resource is shown.
type Kind string

const (
	// KindText is also the fallback for anything unclaimed: a text viewer that
	// notices binary content is more useful than a "cannot preview" viewer for
	// every unknown extension.
	KindText     Kind = "text"
	KindMarkdown Kind = "markdown"
	KindTable    Kind = "table"
	KindImage    Kind = "image"
	KindPDF      Kind = "pdf"
	KindHTML     Kind = "html"
	KindDir      Kind = "dir"
)

// Viewer is one entry of the registry.
type Viewer struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// Patterns are globs the resource's file name is matched against. A pattern
	// containing no "/" is matched against the base name only; one containing a
	// "/" is matched against the whole path inside the scope.
	//
	// `*` matches a run of characters except "/", `**` matches any run
	// including "/", `?` matches one character except "/", and `[...]` matches a
	// character class. Matching is case-insensitive: a viewer that treated
	// README.MD differently from readme.md would be surprising.
	//
	// They are part of the published answer (ADR-016's 认领规则), so a plugin
	// that wants to claim addresses the same way can read them instead of
	// guessing.
	Patterns []string `json:"patterns,omitempty"`
	// Priority decides claims when several viewers match. Higher wins; ties go
	// to the earlier declaration.
	Priority int `json:"priority,omitempty"`
}

// Registry is an ordered set of viewers.
//
// It exists because "what is this file" must have exactly one answer, and the
// answer must come from one place: the viewer page and the JSON API both ask
// this registry (ADR-016's 认领优先级).
type Registry struct {
	viewers []Viewer
}

// Default is the first batch of viewers (roadmap Phase 5).
//
// The order of the slice is the tie-break for equal priority, so specific
// types are declared before the text fallback that would otherwise claim
// everything.
func Default() *Registry {
	return &Registry{viewers: []Viewer{
		{
			ID: "dir", Name: "目录", Kind: KindDir, Priority: 100,
		},
		{
			ID: "image", Name: "图片", Kind: KindImage, Priority: 50,
			Patterns: []string{"*.png", "*.jpg", "*.jpeg", "*.gif", "*.webp", "*.bmp", "*.svg", "*.ico", "*.avif"},
		},
		{
			ID: "pdf", Name: "PDF", Kind: KindPDF, Priority: 50,
			Patterns: []string{"*.pdf"},
		},
		{
			ID: "html", Name: "HTML（sandbox）", Kind: KindHTML, Priority: 50,
			Patterns: []string{"*.html", "*.htm"},
		},
		{
			ID: "markdown", Name: "Markdown", Kind: KindMarkdown, Priority: 40,
			Patterns: []string{"*.md", "*.markdown", "*.mdown", "*.mkd", "*.mkdn"},
		},
		{
			ID: "table", Name: "表格", Kind: KindTable, Priority: 30,
			Patterns: []string{"*.csv", "*.tsv", "*.tab"},
		},
		{
			ID: "text", Name: "文本", Kind: KindText, Priority: 0,
			Patterns: []string{"*"},
		},
	}}
}

// All returns the viewers in claim order (used by the "open as" switcher).
func (r *Registry) All() []Viewer {
	if r == nil {
		return nil
	}
	out := append([]Viewer(nil), r.viewers...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}

// Lookup returns a viewer by id, for an explicit override.
func (r *Registry) Lookup(id string) (Viewer, bool) {
	if r == nil {
		return Viewer{}, false
	}
	id = strings.TrimSpace(id)
	for _, v := range r.viewers {
		if v.ID == id {
			return v, true
		}
	}
	return Viewer{}, false
}

// Claim decides which viewer shows a resource.
//
// A directory is always the directory viewer (a pattern cannot override what
// the filesystem says); otherwise the highest-priority matching viewer wins,
// and the text fallback catches everything else.
func (r *Registry) Claim(path string, isDir bool) Viewer {
	if r == nil {
		return Viewer{ID: "text", Name: "文本", Kind: KindText}
	}
	if isDir {
		if v, ok := r.Lookup("dir"); ok {
			return v
		}
		return Viewer{ID: "dir", Name: "目录", Kind: KindDir}
	}
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	ordered := r.All()
	for _, v := range ordered {
		if v.Kind == KindDir {
			continue
		}
		for _, pat := range v.Patterns {
			// A pattern with a separator is matched against the whole path, so
			// `docs/*.md` can be claimed differently from a stray README at the
			// root.
			target := base
			if strings.Contains(pat, "/") {
				target = path
			}
			if Match(pat, target) {
				return v
			}
		}
	}
	// A registry without its text entry still has to answer something.
	return Viewer{ID: "text", Name: "文本", Kind: KindText}
}

// Match reports whether name matches a glob pattern (case-insensitively).
func Match(pattern, name string) bool {
	return matchGlob(strings.ToLower(pattern), strings.ToLower(name))
}

// matchGlob is the glob matcher described on Viewer.Patterns. It is written out
// rather than compiled to a regexp because the character-class rules here are
// narrower than a regexp's (a class never matches "/").
func matchGlob(p, n string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			if strings.HasPrefix(p, "**") {
				rest := p[2:]
				// "**/" also matches zero directories, so "**/*.md" claims a
				// top-level "a.md".
				if strings.HasPrefix(rest, "/") && matchGlob(rest[1:], n) {
					return true
				}
				for i := 0; i <= len(n); i++ {
					if matchGlob(rest, n[i:]) {
						return true
					}
				}
				return false
			}
			rest := p[1:]
			for i := 0; ; i++ {
				if matchGlob(rest, n[i:]) {
					return true
				}
				if i >= len(n) || n[i] == '/' {
					return false
				}
			}
		case '?':
			if len(n) == 0 || n[0] == '/' {
				return false
			}
			p, n = p[1:], n[1:]
		case '[':
			ok, next, consumed := matchClass(p, n)
			if !ok {
				return false
			}
			p, n = next, n[consumed:]
		default:
			if len(n) == 0 || p[0] != n[0] {
				return false
			}
			p, n = p[1:], n[1:]
		}
	}
	return len(n) == 0
}

// matchClass matches a `[...]` character class at the front of p against the
// first character of n, returning the class's end and whether it matched.
//
// A malformed or unterminated class is treated as a literal `[`, which is what
// a user writing a stray bracket in a pattern means.
func matchClass(p, n string) (bool, string, int) {
	end := strings.IndexByte(p, ']')
	if end < 0 {
		return len(n) > 0 && n[0] == '[', p[1:], 1
	}
	if end == 1 { // "[]" is not a class
		return len(n) > 0 && n[0] == '[', p[1:], 1
	}
	body := p[1:end]
	negate := false
	if body[0] == '!' || body[0] == '^' {
		negate = true
		body = body[1:]
	}
	if len(n) == 0 || n[0] == '/' {
		return false, p[end+1:], 0
	}
	c := n[0]
	inClass := false
	for i := 0; i < len(body); i++ {
		if i+2 < len(body) && body[i+1] == '-' {
			if c >= body[i] && c <= body[i+2] {
				inClass = true
			}
			i += 2
			continue
		}
		if body[i] == c {
			inClass = true
		}
	}
	if inClass == negate {
		return false, p[end+1:], 0
	}
	return true, p[end+1:], 1
}
