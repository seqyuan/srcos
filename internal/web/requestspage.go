package web

import (
	"strings"

	"github.com/seqyuan/srcos/internal/accessrequest"
)

// RequestsPage renders a user's own tool-access requests (B3): what they asked
// for and what an administrator decided. Server-rendered, like the rest of the
// reading surfaces.
func RequestsPage(siteTitle, username string, reqs []accessrequest.Request) string {
	var b strings.Builder
	b.WriteString(requestCSS)
	b.WriteString(`<main class="wrap req"><p class="crumb"><a href="/tools">工具</a> / 我的申请</p><h1>我的申请</h1>`)

	if len(reqs) == 0 {
		b.WriteString(`<p class="muted">还没有申请。在 <a href="/tools">工具</a> 页对「可申请」的工具提交一个。</p></main>`)
		return PageShell(siteTitle, "我的申请", b.String())
	}

	b.WriteString(`<table class="req-table"><thead><tr>`)
	b.WriteString(`<th>提交时间</th><th>工具</th><th>状态</th><th>用途</th><th>决定</th>`)
	b.WriteString(`</tr></thead><tbody>`)
	for _, r := range reqs {
		b.WriteString(requestRow(r))
	}
	b.WriteString(`</tbody></table></main>`)
	return PageShell(siteTitle, "我的申请", b.String())
}

func requestRow(r accessrequest.Request) string {
	state := `<span class="req-pending">待审批</span>`
	switch r.State {
	case accessrequest.Approved:
		state = `<span class="req-ok">已批准</span>`
	case accessrequest.Denied:
		state = `<span class="req-bad">已拒绝</span>`
	}

	decision := ""
	if !r.DecidedAt.IsZero() {
		decision = esc(r.DecidedBy) + ` <span class="req-muted">· ` + esc(r.DecidedAt.Local().Format("2006-01-02 15:04")) + `</span>`
		if r.Note != "" {
			decision += `<div class="req-note">` + esc(r.Note) + `</div>`
		}
	}

	return `<tr>` +
		`<td class="req-ts">` + esc(r.CreatedAt.Local().Format("2006-01-02 15:04")) + `</td>` +
		`<td><code>` + esc(r.Tool) + `</code></td>` +
		`<td>` + state + `</td>` +
		`<td>` + esc(r.Reason) + `</td>` +
		`<td>` + decision + `</td>` +
		`</tr>`
}

const requestCSS = `<style>
  .req { max-width: 900px; }
  .req-table { width: 100%; border-collapse: collapse; font-size: 13px; }
  .req-table th {
    text-align: left; font-weight: 600; color: var(--text-muted); font-size: 11.5px;
    text-transform: uppercase; letter-spacing: 0.04em; padding: 10px 12px; border-bottom: 1px solid var(--border);
  }
  .req-table td { padding: 10px 12px; border-bottom: 1px solid var(--border); vertical-align: top; }
  .req-ts { white-space: nowrap; color: var(--text-muted); font-variant-numeric: tabular-nums; }
  .req-ok { color: #1a7f37; font-weight: 600; }
  .req-bad { color: #d33; font-weight: 600; }
  .req-pending { color: #9a6700; font-weight: 600; }
  .req-note { color: var(--text-muted); margin-top: 4px; }
  .req-muted { color: var(--text-muted); }
</style>`
