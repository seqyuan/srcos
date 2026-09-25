package web

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/audit"
)

// AuditPage renders the audit console: the structured stream, newest first,
// with the same filters the CLI has. It is server-rendered (a plain GET form,
// no JavaScript) so an operator can read it from anything that speaks HTML.
func AuditPage(siteTitle, username string, events []audit.Event, f audit.Filter, problems []audit.Problem, forward audit.ForwardStatus) string {
	var b strings.Builder

	b.WriteString(auditCSS)
	b.WriteString(`<main class="audit-wrap">`)
	b.WriteString(`<div class="audit-head"><div><div class="audit-title">审计流</div>`)
	b.WriteString(`<div class="audit-sub">谁、何时、哪个版本的工具、什么参数、被允许还是被拒绝。`)
	b.WriteString(`存储于 <code>data/audit/audit-YYYY-MM-DD.jsonl</code>（只追加、按天轮转）。`)
	b.WriteString(`命令行等价入口：<code>srcos audit list</code>。</div></div>`)
	b.WriteString(`<div class="audit-actions">`)
	b.WriteString(`<a class="btn" href="/admin">管理控制台</a>`)
	b.WriteString(`<span class="audit-muted">` + esc(username) + `</span>`)
	b.WriteString(`</div></div>`)

	// Tamper-evidence status (ADR-024): the per-file hash chain. An admin should
	// see at a glance whether the stream they are reading still verifies.
	if len(problems) == 0 {
		b.WriteString(`<div class="audit-verified">链校验通过（每行 hash 与前一行相连）</div>`)
	} else {
		b.WriteString(`<div class="audit-broken">链校验发现 ` + fmt.Sprint(len(problems)) + ` 处异常：`)
		for i, p := range problems {
			if i == 5 {
				b.WriteString(` …`)
				break
			}
			where := p.File
			if p.Line > 0 {
				where = fmt.Sprintf("%s:%d", p.File, p.Line)
			}
			b.WriteString(`<code>` + esc(where) + ` ` + esc(p.Reason) + `</code> `)
		}
		b.WriteString(`</div>`)
	}

	// External sink status: an operator has to see "the collector is down and N
	// events are queued" — silence is the one thing this must not do.
	if forward.Configured {
		msg := fmt.Sprintf("审计外发：%s · 待发送 %d", forward.Sink, forward.Unsent)
		cls := "audit-verified"
		if forward.Unsent > 0 || forward.Dropped > 0 {
			cls = "audit-broken"
		}
		if forward.Dropped > 0 {
			msg += fmt.Sprintf(" · 已丢弃 %d（spool 超过上限）", forward.Dropped)
		}
		b.WriteString(`<div class="` + cls + `">` + esc(msg) + `</div>`)
	}

	// Filter form. GET so the query string is bookmarkable and shareable.
	b.WriteString(`<form class="audit-filter" method="get" action="/admin/audit">`)
	b.WriteString(`<input type="text" name="user" placeholder="用户" value="` + esc(f.User) + `">`)
	b.WriteString(`<select name="action">` + actionOptions(f.Action) + `</select>`)
	b.WriteString(`<select name="decision">` + decisionOptions(f.Decision) + `</select>`)
	b.WriteString(`<input type="text" name="since" placeholder="since，如 24h / 7d" value="` + esc(sinceText(f)) + `">`)
	b.WriteString(`<button class="btn" type="submit">筛选</button>`)
	b.WriteString(`<span class="audit-muted">共 ` + fmt.Sprint(len(events)) + ` 条</span>`)
	b.WriteString(`</form>`)

	if len(events) == 0 {
		b.WriteString(`<div class="audit-empty">没有匹配的审计记录。</div>`)
		b.WriteString(`</main>`)
		return PageShell(siteTitle, "审计流", b.String())
	}

	b.WriteString(`<table class="audit"><thead><tr>`)
	b.WriteString(`<th>时间</th><th>结果</th><th>动作</th><th>行为者</th><th>目标</th><th>详情</th>`)
	b.WriteString(`</tr></thead><tbody>`)
	// Newest first: an operator reading the page wants the latest first, while
	// the file and the CLI stay oldest-first.
	for i := len(events) - 1; i >= 0; i-- {
		b.WriteString(auditRow(events[i]))
	}
	b.WriteString(`</tbody></table></main>`)

	return PageShell(siteTitle, "审计流", b.String())
}

func auditRow(e audit.Event) string {
	decision := `<span class="audit-ok">allow</span>`
	if e.Decision == audit.Deny {
		decision = `<span class="audit-bad">deny</span>`
	}
	target := ""
	if e.Target.Type != "" || e.Target.ID != "" {
		target = esc(e.Target.Type) + " " + esc(e.Target.ID)
		if e.Target.Version != "" {
			target += "@" + esc(e.Target.Version)
		}
	}

	var details strings.Builder
	if e.Reason != "" {
		details.WriteString(`<div class="audit-reason">` + esc(e.Reason) + `</div>`)
	}
	for _, k := range auditSortedKeys(e.Refs) {
		details.WriteString(`<code>` + esc(k) + `=` + esc(e.Refs[k]) + `</code> `)
	}
	if len(e.Params) > 0 {
		var ps []string
		for _, k := range auditSortedKeys(e.Params) {
			ps = append(ps, esc(k)+"="+esc(e.Params[k]))
		}
		details.WriteString(`<div class="audit-muted">{` + strings.Join(ps, " ") + `}</div>`)
	}
	if e.Request != nil && e.Request.IP != "" {
		details.WriteString(`<div class="audit-muted">` +
			esc(e.Request.Method+" "+e.Request.Path+" from "+e.Request.IP) + `</div>`)
	}

	return `<tr>` +
		`<td class="audit-ts">` + esc(e.TS.Local().Format("2006-01-02 15:04:05")) + `</td>` +
		`<td>` + decision + `</td>` +
		`<td><code>` + esc(e.Action) + `</code></td>` +
		`<td>` + esc(auditActorLabel(e.Actor)) + `</td>` +
		`<td>` + target + `</td>` +
		`<td>` + details.String() + `</td>` +
		`</tr>`
}

func auditActorLabel(a audit.Actor) string {
	who := a.User
	if who == "" {
		who = "anonymous"
	}
	switch a.Kind {
	case audit.KindAgentToken:
		who += "（agent token"
		if a.TokenID != "" {
			who += " " + a.TokenID
		}
		who += "）"
	case audit.KindCLI:
		who += "（cli）"
	case audit.KindSystem:
		who += "（system）"
	}
	return who
}

// auditActions are the values worth offering in the dropdown (the write path,
// denials and the configuration changes an auditor looks for).
var auditActions = []string{
	"submit", "cancel", "run_flow",
	"csrf", "auth", "scope",
	"grant.set", "grant.remove", "grant.allow", "grant.deny", "group.set", "admins.set",
	"token.create", "token.revoke",
	"request.create", "request.approve", "request.deny",
	"agenttoken.mint", "agenttoken.revoke",
	"instance.done", "instance.settled", "instance.stopped",
	"instance.started", "instance.reaped", "instance.adopted", "instance.orphaned",
}

func actionOptions(selected string) string {
	var b strings.Builder
	b.WriteString(`<option value="">全部动作</option>`)
	for _, a := range auditActions {
		sel := ""
		if a == selected {
			sel = " selected"
		}
		b.WriteString(`<option value="` + esc(a) + `"` + sel + `>` + esc(a) + `</option>`)
	}
	return b.String()
}

func decisionOptions(selected string) string {
	opt := func(v, label string) string {
		sel := ""
		if v == selected {
			sel = " selected"
		}
		return `<option value="` + v + `"` + sel + `>` + label + `</option>`
	}
	return opt("", "全部结果") + opt(audit.Allow, "allow") + opt(audit.Deny, "deny")
}

// sinceText renders the filter's cutoff back into the form, as a coarse
// duration so the value survives a reload without a timestamp zoo.
func sinceText(f audit.Filter) string {
	if f.Since.IsZero() {
		return ""
	}
	d := time.Since(f.Since)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()+0.5))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24+0.5))
	}
}

func auditSortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// auditCSS is scoped to this page: PageShell carries only base.css, and the
// console's own table classes live inside admin.html's style block.
const auditCSS = `<style>
  .audit-wrap { max-width: 1180px; margin: 0 auto; padding: 20px var(--pad-x) 64px; }
  .audit-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; }
  .audit-title { font-size: 20px; font-weight: 650; letter-spacing: -0.01em; }
  .audit-sub { color: var(--text-muted); font-size: 13px; margin: 4px 0 18px; }
  .audit-actions { display: flex; gap: 8px; align-items: center; }
  .audit-filter { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin-bottom: 14px; }
  .audit-filter input, .audit-filter select {
    height: 34px; padding: 0 10px; border: 1px solid var(--border); border-radius: 8px;
    background: var(--card, transparent); color: var(--text); font-size: 13px;
  }
  .audit { width: 100%; border-collapse: collapse; font-size: 13px; }
  .audit th {
    text-align: left; font-weight: 600; color: var(--text-muted); font-size: 11.5px;
    text-transform: uppercase; letter-spacing: 0.04em; padding: 10px 12px; border-bottom: 1px solid var(--border);
  }
  .audit td { padding: 10px 12px; border-bottom: 1px solid var(--border); vertical-align: top; }
  .audit tr:hover td { background: var(--hover); }
  .audit-ts { white-space: nowrap; font-variant-numeric: tabular-nums; color: var(--text-muted); }
  .audit-ok { color: #1a7f37; font-weight: 600; }
  .audit-bad { color: #d33; font-weight: 600; }
  .audit-reason { color: #d33; }
  .audit-muted { color: var(--text-muted); }
  .audit-empty { padding: 22px 12px; color: var(--text-muted); font-size: 13px; }
  .audit-verified { color: #1a7f37; font-size: 12.5px; margin-bottom: 12px; }
  .audit-broken { color: #d33; font-size: 12.5px; margin-bottom: 12px; }
</style>`
