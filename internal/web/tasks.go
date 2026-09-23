package web

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/seqyuan/srcos/internal/inspect"
)

// This file renders the task list and one instance's detail page.
//
// Two properties shape it:
//
//   - **It is the user's own view.** The list is scoped server-side to the
//     caller (inspect.Instances), exactly like /api/jobs, so the page cannot
//     disagree with the API about what someone may see.
//   - **It works without JavaScript.** The list and the metadata are rendered
//     here; the log tail is rendered here too. The live stream is progressive
//     enhancement — an EventSource that appends lines — not a requirement to
//     read the page (ADR-012 keeps this a Go template for that reason).
//
// The log panel is also where the viewer pays off: every artifact is a link
// into /view, so "the job finished" leads to "look at what it produced" without
// a detour through a shell.

// taskStateMeta is the display half of a lifecycle state. The mappings live in
// one place and are shipped to the page as JSON, so the JavaScript that updates
// the badge on a state event does not restate them (ADR-018's spirit: one
// definition, several consumers).
type taskStateMeta struct {
	Label string `json:"label"`
	Class string `json:"cls"`
}

var taskStates = map[string]taskStateMeta{
	"pending":   {"排队中", "warn"},
	"starting":  {"启动中", "warn"},
	"running":   {"运行中", "live"},
	"idle":      {"空闲", "live"},
	"submitted": {"已提交（等待完成）", "warn"},
	"stopping":  {"停止中", "warn"},
	"succeeded": {"成功", "ok"},
	"failed":    {"失败", "bad"},
	"stopped":   {"已停止", "warn"},
}

func stateMeta(state string) taskStateMeta {
	if m, ok := taskStates[state]; ok {
		return m
	}
	return taskStateMeta{Label: state, Class: ""}
}

// TasksPage lists the user's instances, newest first.
func TasksPage(siteTitle string, views []inspect.InstanceView, tools []string, toolFilter, kindFilter string) string {
	var b strings.Builder
	b.WriteString(tasksCSS)
	b.WriteString(`<main class="wrap tk">`)
	b.WriteString(`<p class="crumb"><a href="/">仪表盘</a> / 任务</p>`)
	b.WriteString(`<h1>任务</h1>`)

	live := 0
	for _, v := range views {
		if !stateFinished(v.State) {
			live++
		}
	}
	fmt.Fprintf(&b, `<p class="muted">共 %d 个实例，其中 %d 个未结束。点任务名看详情与实时日志。</p>`, len(views), live)

	// Filters as a plain GET form: no JavaScript, and the URL is shareable.
	// A 30-second reload is offered rather than imposed, because it would
	// otherwise reset the scroll position while someone reads the list.
	b.WriteString(`<form class="tk-filter" method="GET" action="/tasks">`)
	fmt.Fprintf(&b, `<label>工具 <select name="tool">%s</select></label>`,
		selectOptions(append([]string{""}, tools...), append([]string{"所有工具"}, tools...), toolFilter))
	fmt.Fprintf(&b, `<label>类型 <select name="kind">%s</select></label>`,
		selectOptions([]string{"", "task", "service"}, []string{"全部", "任务", "常驻服务"}, kindFilter))
	b.WriteString(`<button type="submit">筛选</button><a href="/tasks">清除</a></form>`)

	if len(views) == 0 {
		b.WriteString(`<p class="muted tk-empty">还没有任务。用 <a href="/tools">工具</a> 页提交一个，或执行
<code>srcos job submit</code>。</p></main>`)
		return PageShell(siteTitle, "任务", b.String())
	}

	b.WriteString(`<div class="tk-card"><table class="tk-table"><thead><tr>
  <th>状态</th><th>工具</th><th>任务</th><th>时长</th><th>退出码</th><th>开始</th><th>产物</th><th></th>
</tr></thead><tbody>`)
	for _, v := range views {
		m := stateMeta(v.State)
		detail := "/tasks/" + url.PathEscape(v.ID)
		name := v.Name
		if name == "" {
			name = v.ID
		}
		exit := "—"
		if v.ExitCode != 0 {
			exit = fmt.Sprintf("%d", v.ExitCode)
		}
		duration := v.Duration
		if duration == "" {
			duration = "—"
		}
		outputs := "—"
		if len(v.Outputs) > 0 {
			outputs = fmt.Sprintf("%d", len(v.Outputs))
		}
		fmt.Fprintf(&b, `<tr>
  <td><span class="tk-tag %s">%s</span></td>
  <td><a href="/tools/%s">%s</a></td>
  <td><a href="%s">%s</a></td>
  <td class="mono">%s</td>
  <td class="mono">%s</td>
  <td class="mono">%s</td>
  <td class="mono">%s</td>
  <td><a href="%s#log">日志</a></td>
</tr>`,
			esc(m.Class), esc(m.Label),
			esc(v.Tool), esc(v.Tool),
			esc(detail), esc(name),
			esc(duration), esc(exit), esc(humanTime(v.StartedAt)), esc(outputs),
			esc(detail))
	}
	b.WriteString(`</tbody></table></div></main>`)
	return PageShell(siteTitle, "任务", b.String())
}

// TaskPage renders one instance: what it is, what it produced, and its log.
func TaskPage(siteTitle string, view inspect.InstanceView, artifacts []inspect.Artifact, logTail string) string {
	var b strings.Builder
	b.WriteString(tasksCSS)
	b.WriteString(`<main class="wrap tk">`)
	b.WriteString(`<p class="crumb"><a href="/">仪表盘</a> / <a href="/tasks">任务</a> / ` + esc(view.ID) + `</p>`)

	m := stateMeta(view.State)
	name := view.Name
	if name == "" {
		name = view.ID
	}
	fmt.Fprintf(&b, `<h1>%s <span class="tk-tag %s" id="state-tag">%s</span></h1>`, esc(name), esc(m.Class), esc(m.Label))

	// Metadata: only what this instance actually has (a task has no endpoint,
	// a service has no exit code).
	rows := [][2]string{
		{"实例", view.ID},
		{"工具", view.Tool},
		{"类型", view.Kind},
		{"后端", view.Backend},
		{"沙箱", view.Sandbox},
		{"资源限制", view.Limiter},
		{"开始", humanTime(view.StartedAt)},
		{"时长", view.Duration},
	}
	if view.Kind == "task" {
		rows = append(rows, [2]string{"退出码", fmt.Sprintf("%d", view.ExitCode)})
	}
	if view.Endpoint != "" {
		rows = append(rows, [2]string{"端点", view.Endpoint})
	}
	if view.RoutePath != "" {
		rows = append(rows, [2]string{"路由", view.RoutePath})
	}
	if view.Error != "" {
		rows = append(rows, [2]string{"错误", view.Error})
	}
	b.WriteString(`<div class="tk-card tk-meta"><dl>`)
	for _, r := range rows {
		b.WriteString(`<dt>` + esc(r[0]) + `</dt><dd>` + esc(r[1]) + `</dd>`)
	}
	if len(view.Tags) > 0 {
		keys := make([]string, 0, len(view.Tags))
		for k := range view.Tags {
			keys = append(keys, k)
		}
		sortStrings(keys)
		for _, k := range keys {
			b.WriteString(`<dt>` + esc(k) + `</dt><dd>` + esc(view.Tags[k]) + `</dd>`)
		}
	}
	b.WriteString(`</dl></div>`)

	// Artifacts: the point of running anything. A sandbox path the viewer can
	// open gets a link; the rest still gets its path, because an agent or a
	// human may need to copy it.
	if len(artifacts) > 0 {
		b.WriteString(`<h2>产物</h2><div class="tk-card"><table class="tk-table"><thead><tr>
  <th>路径</th><th>状态</th><th>大小</th><th></th></tr></thead><tbody>`)
		for _, a := range artifacts {
			status := `<span class="tk-tag ok">存在</span>`
			switch {
			case !a.Exists:
				note := a.Note
				if note == "" {
					note = "未找到"
				}
				status = `<span class="tk-tag bad">` + esc(note) + `</span>`
			case a.IsDir:
				status = fmt.Sprintf(`<span class="tk-tag ok">目录（%d 项）</span>`, a.Entries)
			}
			size := "—"
			if a.Exists && !a.IsDir {
				size = humanBytes(a.Size)
			}
			link := ""
			if a.Exists && a.Addr != "" {
				link = fmt.Sprintf(`<a href="/view?src=%s">预览</a>`, url.QueryEscape(a.Addr))
			}
			fmt.Fprintf(&b, `<tr><td class="mono">%s</td><td>%s</td><td class="mono">%s</td><td>%s</td></tr>`,
				esc(a.Path), status, esc(size), link)
		}
		b.WriteString(`</tbody></table></div>`)
	} else {
		b.WriteString(`<h2>产物</h2><p class="muted">这个提交没有声明产物。</p>`)
	}

	// The log panel. The tail is rendered here so the page is useful without
	// JavaScript; the script replaces it with the live stream when it can.
	b.WriteString(`<h2>日志</h2><div class="tk-card tk-log">`)
	fmt.Fprintf(&b, `<div class="tk-log-head">
  <span class="tk-live" id="live-ind">● 实时</span>
  <a href="/api/jobs/%s/logs" target="_blank" rel="noopener noreferrer">完整尾部</a>
  <label class="tk-auto"><input type="checkbox" id="autoscroll" checked> 自动滚动</label>
</div>`, esc(url.PathEscape(view.ID)))
	if strings.TrimSpace(logTail) == "" {
		b.WriteString(`<pre class="tk-log-pre" id="log">（暂无输出）</pre>`)
	} else {
		b.WriteString(`<pre class="tk-log-pre" id="log">` + esc(logTail) + `</pre>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<noscript><p class="muted tk-empty">上面的日志是打开本页时的最后 200 行；实时跟随需要 JavaScript。</p></noscript>`)
	b.WriteString(`</main>`)

	boot := marshalScriptJSON(map[string]any{
		"id":     view.ID,
		"states": taskStates,
	})
	script := `<script>window.__SRCOS_TASK__ = ` + boot + `;</script><script>` + taskLogScript + `</script>`
	return PageShell(siteTitle, name, b.String()+script)
}

// selectOptions renders a <select>'s options. values and labels run in
// parallel; an empty or missing label falls back to the value.
func selectOptions(values, labels []string, selected string) string {
	var b strings.Builder
	for i, v := range values {
		label := v
		if i < len(labels) && labels[i] != "" {
			label = labels[i]
		}
		sel := ""
		if v == selected {
			sel = " selected"
		}
		fmt.Fprintf(&b, `<option value="%s"%s>%s</option>`, esc(v), sel, esc(label))
	}
	return b.String()
}

// stateFinished reports whether a state is terminal. It mirrors
// runtime.State.Terminal, kept here because the page counts "unfinished" for a
// sentence and importing the runtime for that would be a wider dependency than
// the fact is worth.
func stateFinished(state string) bool {
	switch state {
	case "succeeded", "failed", "stopped":
		return true
	}
	return false
}

func humanTime(rfc3339 string) string {
	if rfc3339 == "" {
		return "—"
	}
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// taskLogScript is the live-log client. It is deliberately small: connect, append
// `line` events, follow `state`/`end`, and stop. Everything it needs to know
// about the vocabulary arrives in __SRCOS_TASK__.
const taskLogScript = `
(function () {
  var boot = window.__SRCOS_TASK__ || {};
  var pre = document.getElementById('log');
  var ind = document.getElementById('live-ind');
  var auto = document.getElementById('autoscroll');
  var tag = document.getElementById('state-tag');
  if (!pre) return;
  // Without EventSource (or without an id) the server-rendered tail stands.
  if (!boot.id || !window.EventSource) {
    if (ind) ind.textContent = '● 静态';
    return;
  }

  // A reconnect must not duplicate lines: the server replays the tail on every
  // connection, so the first line after an open replaces what is on screen.
  var reset = true;
  var es = new EventSource('/api/jobs/' + encodeURIComponent(boot.id) + '/logs?follow=1&tail=200');

  es.addEventListener('open', function () { reset = true; });
  es.addEventListener('line', function (ev) {
    if (reset) { pre.textContent = ''; reset = false; }
    pre.appendChild(document.createTextNode(ev.data + '\n'));
    if (!auto || auto.checked) pre.scrollTop = pre.scrollHeight;
  });
  es.addEventListener('state', function (ev) {
    var meta = (boot.states || {})[ev.data] || { label: ev.data, cls: '' };
    if (tag) { tag.textContent = meta.label; tag.className = 'tk-tag ' + (meta.cls || ''); }
  });
  es.addEventListener('note', function (ev) {
    if (ind) ind.textContent = '● ' + ev.data;
  });
  es.addEventListener('end', function () {
    if (ind) ind.textContent = '● 日志结束';
    es.close();
  });
  es.addEventListener('error', function () {
    // EventSource retries on its own; say what the state is so the reader is
    // not left watching a silent panel.
    if (ind) ind.textContent = '● 连接中断（重试中）';
  });
})();`

// tasksCSS styles the task pages. Injected with the markup that uses it, the
// same way the resource viewer does.
const tasksCSS = `<style>
.tk h1 { font-size: 20px; margin: 0 0 6px; }
.tk h2 { font-size: 15px; margin: 26px 0 8px; }
.tk .crumb { font-size: 13px; }
.tk .crumb a { color: var(--accent); text-decoration: none; }
.tk .crumb a:hover { text-decoration: underline; }
.tk-filter { display: flex; gap: 12px; align-items: flex-end; flex-wrap: wrap; margin: 14px 0 18px; }
.tk-filter label { font-size: 12px; color: var(--text-muted); display: grid; gap: 4px; }
.tk-filter select { height: 34px; padding: 0 10px; border-radius: var(--r-chip); border: 1px solid var(--border); background: var(--bg); color: var(--text); font-size: 13px; }
.tk-filter button { height: 34px; padding: 0 16px; border-radius: var(--r-pill); border: 1px solid var(--border); background: var(--bg-panel); color: var(--text); font-size: 13px; cursor: pointer; }
.tk-filter button:hover { background: var(--hover); }
.tk-filter a { font-size: 13px; color: var(--accent); text-decoration: none; padding-bottom: 8px; }
.tk-card { background: var(--bg-panel); border: 1px solid var(--border); border-radius: var(--r-card); overflow: hidden; box-shadow: var(--sh-card); }
.tk-table { width: 100%; border-collapse: collapse; font-size: 13px; }
.tk-table th { text-align: left; font-weight: 600; color: var(--text-muted); font-size: 11.5px; text-transform: uppercase; letter-spacing: .04em; padding: 10px 14px; border-bottom: 1px solid var(--border); white-space: nowrap; }
.tk-table td { padding: 9px 14px; border-bottom: 1px solid var(--border); vertical-align: middle; }
.tk-table tr:last-child td { border-bottom: none; }
.tk-table tbody tr:hover td { background: var(--hover); }
.tk-table a { color: var(--accent); text-decoration: none; }
.tk-table a:hover { text-decoration: underline; }
.mono { font-family: var(--mono); font-size: 12px; }
.tk-tag { display: inline-block; padding: 2px 9px; border-radius: var(--r-pill); font-size: 11.5px; font-weight: 600; border: 1px solid var(--border); color: var(--text-muted); white-space: nowrap; }
.tk-tag.ok { color: #1a7f37; border-color: color-mix(in srgb, #1a7f37 35%, transparent); background: color-mix(in srgb, #1a7f37 10%, transparent); }
.tk-tag.bad { color: var(--danger); border-color: color-mix(in srgb, var(--danger) 35%, transparent); background: color-mix(in srgb, var(--danger) 10%, transparent); }
.tk-tag.warn { color: #9a6700; border-color: color-mix(in srgb, #9a6700 35%, transparent); background: color-mix(in srgb, #9a6700 10%, transparent); }
.tk-tag.live { color: var(--accent); border-color: color-mix(in srgb, var(--accent) 35%, transparent); background: var(--accent-bg); }
.dark .tk-tag.ok { color: #4cc46a; }
.dark .tk-tag.warn { color: #e3b341; }
.tk-meta dl { display: grid; grid-template-columns: max-content 1fr; gap: 6px 18px; margin: 0; padding: 14px 18px; font-size: 13px; }
.tk-meta dt { color: var(--text-muted); }
.tk-meta dd { margin: 0; font-family: var(--mono); font-size: 12.5px; word-break: break-all; }
.tk-empty { margin-top: 14px; }
.tk-log-head { display: flex; gap: 16px; align-items: center; padding: 10px 14px; border-bottom: 1px solid var(--border); font-size: 12.5px; }
.tk-log-head a { color: var(--accent); text-decoration: none; }
.tk-log-head a:hover { text-decoration: underline; }
.tk-auto { margin-left: auto; display: flex; align-items: center; gap: 6px; color: var(--text-muted); }
.tk-live { color: var(--accent); }
.tk-log-pre { margin: 0; padding: 12px 16px; max-height: 62vh; overflow: auto; background: transparent; font-family: var(--mono); font-size: 12.5px; line-height: 1.55; white-space: pre-wrap; word-break: break-all; }
</style>`
