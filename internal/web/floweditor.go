package web

import (
	"io/fs"
	"strings"
)

// FlowEditorPage serves the canvas shell.
//
// It is the one page of the gateway that is not a Go template: the frontend is a
// separate Vite project (webui/) whose build is embedded in this binary
// (ADR-012). Rather than duplicating Vite's content-hashed asset names in Go,
// the page *is* the built index.html with a small bootstrap object injected —
// the asset tags stay whatever the build produced.
func FlowEditorPage(siteTitle, username, flowID string) string {
	shell := ""
	if assets := UIAssets(); assets != nil {
		if data, err := fs.ReadFile(assets, "index.html"); err == nil {
			shell = string(data)
		}
	}
	if shell == "" {
		// No build present: this is a normal state (a checkout without Node), so
		// the page explains itself instead of failing. The CLI does everything
		// the canvas does, which is the point of keeping the contract in Go.
		return PageShell(siteTitle, "流程画布", `<style>
  .nc { max-width: 560px; margin: 8vh auto; background: var(--card); border: 1px solid var(--border);
        border-radius: var(--r-lg); padding: 28px; box-shadow: var(--sh-card); }
  .nc h1 { font-size: 18px; margin: 0 0 10px; }
  .nc p { color: var(--text-muted); font-size: 14px; line-height: 1.7; }
  .nc code { background: var(--bg); padding: 2px 6px; border-radius: 6px; }
</style>
<div class="nc">
  <h1>流程画布未构建</h1>
  <p>画布由独立的前端工程（<code>webui/</code>，Vite + React）提供，构建产物嵌入同一个二进制
     （ADR-012：只有这一页加载 React，其余页面仍是 Go 模板）。</p>
  <p>在仓库里运行 <code>make webui</code> 后重新 <code>make build</code> 即可。</p>
  <p>没有 Node 也能用：<code>srcos flow list|validate|run</code>，
     或直接编辑 <code>flow.yaml</code>（契约见 <code>docs/flow-spec.md</code>）。</p>
  <p><a href="/admin">返回管理控制台</a></p>
</div>`)
	}

	// The bootstrap carries what the page cannot know by itself: which flow is
	// being edited, and who is looking at it. It is JSON-escaped and injected as
	// a script, so a flow id can never break out of the string.
	boot := `<script>window.__SRCOS__ = {"flowId":"` + jsonString(flowID) + `","user":"` + jsonString(username) + `"};</script>`
	if i := strings.Index(shell, "</head>"); i >= 0 {
		shell = shell[:i] + boot + shell[i:]
	} else {
		shell = boot + shell
	}
	return shell
}

// jsonString escapes a string for embedding in a JSON literal inside HTML.
func jsonString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '<':
			b.WriteString(`\u003c`)
		case '>':
			b.WriteString(`\u003e`)
		case '&':
			b.WriteString(`\u0026`)
		default:
			if r < 0x20 {
				b.WriteString("")
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
