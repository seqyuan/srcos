package web

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/storage"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file implements the generated fallback UI: a tool catalogue and a form
// derived from a tool's interface.
//
// It is the completion of ADR-017's claim that SRCOS does not need to own tool
// UIs. A tool author who ships only work.sh + interface gets a working page for
// free; one who wants something prettier replaces this page and keeps the same
// signature. That is what makes 不管工具 UI a strategy rather than a gap.
//
// The form is generated from the same `interface` the MCP schema and the canvas
// wiring come from, so an input added to tool.yaml appears in all three without
// any per-surface work (ADR-018).

// ToolsPage lists the tools a user may use.
func ToolsPage(siteTitle, username string, tools []*tool.Tool, requestable []inspect.ToolView) string {
	var b strings.Builder
	b.WriteString(`<main class="wrap"><h1>工具</h1>`)

	if len(tools) == 0 {
		b.WriteString(`<p class="muted">当前没有已注册且对你授权的工具。</p>`)
		if len(requestable) == 0 {
			b.WriteString(`<p class="muted">管理员可以查看 <code>docs/tool-spec.md</code> 了解如何注册一个工具。</p></main>`)
			return PageShell(siteTitle, "工具", b.String())
		}
	} else {
		b.WriteString(`<div class="cards">`)
		for _, t := range tools {
			kind := string(t.Kind)
			if t.Description == "" {
				t.Description = t.Name
			}
			fmt.Fprintf(&b, `<a class="card" href="/tools/%s">
  <div class="card-head"><span class="card-name">%s</span><span class="badge">%s</span></div>
  <div class="card-desc">%s</div>
  <div class="card-meta">%s v%s · backend=%s · sandbox=%s</div>
</a>`,
				esc(t.ID), esc(t.Name), esc(kind), esc(t.Description),
				esc(t.ID), esc(t.Version), esc(string(t.Backend)), esc(string(t.Sandbox)))
		}
		b.WriteString(`</div>`)
	}

	// Tools the user cannot use but may ask for (B3). A separate section on
	// purpose: "you can run this" and "you may ask for this" are different
	// facts, and blending them would misrepresent what the catalogue is.
	if len(requestable) > 0 {
		b.WriteString(toolsRequestableSection(requestable))
		b.WriteString(toolsRequestScript)
	}
	b.WriteString(`</main>`)
	return PageShell(siteTitle, "工具", b.String())
}

func toolsRequestableSection(rs []inspect.ToolView) string {
	var b strings.Builder
	b.WriteString(`<h2 style="margin-top:34px">可申请的工具</h2>`)
	b.WriteString(`<p class="muted">管理员开放了这些工具的申请：你现在没有权限，但可以提交一个申请，由管理员审批。`)
	b.WriteString(`已有的申请与结果见 <a href="/requests">我的申请</a>。</p><div class="cards">`)
	for _, t := range rs {
		desc := t.Description
		if desc == "" {
			desc = t.Name
		}
		fmt.Fprintf(&b, `<div class="card">
  <div class="card-head"><span class="card-name">%s</span><span class="badge">可申请</span></div>
  <div class="card-desc">%s</div>
  <div class="card-meta">%s v%s</div>
  <button type="button" class="btn" style="margin-top:10px" onclick="reqAsk('%s')">申请访问</button>
</div>`, esc(t.Name), esc(desc), esc(t.ID), esc(t.Version), esc(t.ID))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// toolsRequestScript posts a request and sends the user to their list, where
// the outcome will appear.
const toolsRequestScript = `<script>
function reqAsk(tool) {
  var reason = prompt('申请 ' + tool + ' 的访问权限，请说明用途：');
  if (reason === null) { return; }
  fetch('/api/requests', {
    method: 'POST',
    credentials: 'same-origin',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({tool: tool, reason: reason})
  }).then(function(r) {
    return r.json().catch(function(){ return {}; }).then(function(d) {
      if (!r.ok) { alert(d.error || ('HTTP ' + r.status)); return; }
      location.href = '/requests';
    });
  });
}
</script>`

// ToolFormPage renders a form for one tool, built entirely from its interface.
//
// For a `kind: service` tool the page is also its lifecycle surface: the same
// form starts (or replaces) the instance, and a live instance gets an "open"
// link and a stop button. `inst` is the caller's current instance of this tool,
// or nil when there is none (and nil for a task, which has no lifecycle).
func ToolFormPage(siteTitle, username string, t *tool.Tool, storages []storage.Storage, inst *inspect.InstanceView) string {
	var b strings.Builder
	b.WriteString(toolFormCSS)
	b.WriteString(`<main class="wrap">`)
	fmt.Fprintf(&b, `<p class="crumb"><a href="/tools">工具</a> / %s</p>`, esc(t.Name))
	fmt.Fprintf(&b, `<h1>%s <span class="badge">%s</span></h1>`, esc(t.Name), esc(string(t.Kind)))
	if t.Description != "" {
		fmt.Fprintf(&b, `<p class="muted">%s</p>`, esc(t.Description))
	}

	service := t.Kind == tool.KindService
	if service {
		b.WriteString(servicePanel(inst))
	}

	fmt.Fprintf(&b, `<form id="tool-form" data-tool="%s" data-kind="%s">`, esc(t.ID), esc(string(t.Kind)))

	hint := "点「运行」即可。"
	if service {
		hint = "点「启动服务」即可。"
	}
	if len(t.Interface.Inputs) == 0 {
		fmt.Fprintf(&b, `<p class="muted">该工具没有输入参数，%s</p>`, hint)
	}

	for _, in := range t.Interface.Inputs {
		b.WriteString(field(t.ID, in))
	}

	// Resources are exposed too, because a tool's declared ceiling is only a
	// ceiling: the user may lower it (tool-spec §3) and may need to when a
	// cluster is busy.
	fmt.Fprintf(&b, `<fieldset><legend>资源（可留空，用工具默认值 %d 核 / %s / %s）</legend>
  <label>CPU <input type="number" name="__cpu" min="1" max="%d" placeholder="%d"></label>
  <label>内存 <input type="text" name="__mem" placeholder="%s"></label>
  <label>时长 <input type="text" name="__time" placeholder="%s"></label>
</fieldset>`,
		t.Resources.CPU, esc(t.Resources.Memory), esc(t.Resources.Walltime),
		t.Resources.CPU, t.Resources.CPU, esc(t.Resources.Memory), esc(t.Resources.Walltime))

	action := "运行"
	if service {
		action = "启动服务"
	}
	fmt.Fprintf(&b, `<label>任务名 <input type="text" name="__name" placeholder="%s"></label>`, esc(t.Name))
	fmt.Fprintf(&b, `<div class="actions"><button type="submit">%s</button></div></form>`, action)
	b.WriteString(`<div id="result" class="muted"></div></main>`)

	return PageShell(siteTitle, t.Name, b.String()+
		`<script src="/assets/srcos-path-picker.js"></script><script>`+toolFormScript+`</script>`)
}

// servicePanel is the lifecycle half of a service tool page: whether the caller
// has a live instance, where it is reachable, and how to stop it. It is
// rendered server-side so it is correct without JavaScript; only the stop
// button needs a script (the start button is the form's submit).
func servicePanel(inst *inspect.InstanceView) string {
	if inst == nil {
		return `<div class="svc-card"><span class="svc-dot off"></span>未运行。填好参数后点「启动服务」。</div>`
	}
	label, cls := serviceStateLabel(inst.State)
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="svc-card"><span class="svc-dot %s"></span>%s`, cls, esc(label))
	if inst.Error != "" {
		fmt.Fprintf(&b, ` <span class="muted">（%s）</span>`, esc(inst.Error))
	}
	live := !stateFinished(inst.State)
	if live && inst.RoutePath != "" {
		fmt.Fprintf(&b, ` · <a href="%s/">打开服务</a>`, esc(inst.RoutePath))
	}
	if !live {
		fmt.Fprintf(&b, ` · <a href="/tasks/%s">上次运行</a>`, esc(url.PathEscape(inst.ID)))
	} else {
		fmt.Fprintf(&b, ` · <a href="/tasks/%s">详情与日志</a>`, esc(url.PathEscape(inst.ID)))
		fmt.Fprintf(&b, `<button type="button" class="svc-stop" onclick="svcStop('%s')">停止</button>`, esc(inst.ID))
	}
	b.WriteString(`</div>`)
	return b.String()
}

// serviceStateLabel maps a lifecycle state to a Chinese label and a dot class.
// It is the service-page half of the same vocabulary tasks.go ships to its
// live-log client, kept here because this page renders server-side and only
// needs the two strings.
func serviceStateLabel(state string) (string, string) {
	switch state {
	case "running":
		return "运行中", "on"
	case "idle":
		return "空闲", "on"
	case "starting", "pending", "submitted":
		return "启动中", "warn"
	case "stopping":
		return "停止中", "warn"
	case "failed":
		return "启动失败", "bad"
	case "succeeded", "stopped":
		return "已停止", "off"
	}
	return state, "off"
}

// field renders one input as the control its type calls for.
//
// The mapping is the frozen one from tool-spec §2.2; adding a type means adding
// a case here and in the MCP schema generator, which is exactly why that set was
// frozen in Phase 1.
func field(toolID string, in tool.Input) string {
	label := in.Name
	if in.Label != "" {
		label = in.Label
	}
	req := ""
	if in.Required {
		req = " required"
		label += " *"
	}
	desc := ""
	if in.Description != "" {
		desc = fmt.Sprintf(`<div class="hint">%s</div>`, esc(in.Description))
	}
	def := defaultString(in.Default)

	switch in.Type {
	case tool.TypePath, tool.TypeFile, tool.TypeDirectory, tool.TypeDirPath:
		// The primitive control, not a hand-rolled picker: the same element a
		// tool's own UI embeds.
		sel := ""
		switch in.Type {
		case tool.TypeFile:
			sel = "file"
		case tool.TypeDirectory, tool.TypeDirPath:
			sel = "directory"
		default:
			sel = in.Select
		}
		return fmt.Sprintf(
			`<label>%s<srcos-path-picker tool="%s" input="%s" name="%s" value="%s" select="%s"%s></srcos-path-picker>%s</label>`,
			esc(label), esc(toolID), esc(in.Name), esc(in.Name), esc(def), esc(sel), req, desc)

	case tool.TypeBool:
		checked := ""
		if def == "true" {
			checked = " checked"
		}
		return fmt.Sprintf(`<label class="inline"><input type="checkbox" name="%s" value="true"%s> %s%s</label>`,
			esc(in.Name), checked, esc(label), desc)

	case tool.TypeEnum:
		var opts strings.Builder
		for _, v := range in.Values {
			sel := ""
			if v == def {
				sel = " selected"
			}
			fmt.Fprintf(&opts, `<option value="%s"%s>%s</option>`, esc(v), sel, esc(v))
		}
		if !in.Required {
			opts.WriteString(`<option value="">（不设置）</option>`)
		}
		return fmt.Sprintf(`<label>%s<select name="%s"%s>%s</select>%s</label>`,
			esc(label), esc(in.Name), req, opts.String(), desc)

	case tool.TypeInt, tool.TypeFloat:
		step := "1"
		if in.Type == tool.TypeFloat {
			step = "any"
		}
		min, max := "", ""
		if in.Min != nil {
			min = fmt.Sprintf(` min="%v"`, *in.Min)
		}
		if in.Max != nil {
			max = fmt.Sprintf(` max="%v"`, *in.Max)
		}
		return fmt.Sprintf(`<label>%s<input type="number" name="%s" value="%s" step="%s"%s%s%s>%s</label>`,
			esc(label), esc(in.Name), esc(def), step, min, max, req, desc)

	default:
		return fmt.Sprintf(`<label>%s<input type="text" name="%s" value="%s"%s>%s</label>`,
			esc(label), esc(in.Name), esc(def), req, desc)
	}
}

func defaultString(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// toolFormScript collects the form and routes it to the right write endpoint.
//
// It deliberately sends the interface-declared inputs separately from the
// resource overrides, mirroring the job.json shape: a tool's parameters and the
// resources it runs with are different kinds of thing, and only the resources
// are clamped by the server.
//
// The one branch that matters is task vs service: a task is submitted to the
// drop-box queue (POST /api/jobs) and runs to completion; a service is
// instantiated (POST /api/tools/<id>/start) and keeps running. Both go through
// the platform's single write path, so the grant, the submit scope and the
// quota are enforced identically.
var toolFormScript = `
(function () {
  var form = document.getElementById('tool-form');
  var out = document.getElementById('result');
  if (!form) return;
  var toolID = form.dataset.tool;
  var kind = form.dataset.kind;
  var service = kind === 'service';

  // svcStop is the lifecycle page's second verb. It stops the caller's own
  // instance through the same cancel endpoint the task pages and the MCP
  // server use.
  window.svcStop = function (instanceID) {
    if (!confirm('停止这个服务实例？')) return;
    fetch('/api/jobs/' + encodeURIComponent(instanceID) + '/cancel', {
      method: 'POST', credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' }, body: '{}'
    }).then(function (r) {
      return r.json().then(function (b) { return { ok: r.ok, body: b }; });
    }).then(function (r) {
      if (!r.ok) throw new Error(r.body && r.body.error ? r.body.error : '停止失败');
      location.reload();
    }).catch(function (err) { alert('错误：' + (err.message || err)); });
  };

  form.addEventListener('submit', function (ev) {
    ev.preventDefault();
    var fd = new FormData(form);
    var params = {}, res = {};
    fd.forEach(function (value, key) {
      if (key === '__cpu') { if (value) res.cpu = Number(value); return; }
      if (key === '__mem') { if (value) res.memory = String(value); return; }
      if (key === '__time') { if (value) res.walltime = String(value); return; }
      if (key === '__name') { return; }
      // An unchecked checkbox is absent from FormData; anything else present
      // with an empty value means "leave it at the tool's default".
      if (String(value).trim() === '') return;
      params[key] = value;
    });

    var body = { name: fd.get('__name') || '', params: params };
    var url;
    if (service) {
      url = '/api/tools/' + encodeURIComponent(toolID) + '/start';
    } else {
      body.tool = toolID;
      url = '/api/jobs';
    }
    // Both paths accept resource overrides (clamped to the tool's ceiling).
    if (Object.keys(res).length) body.resources = res;

    out.textContent = service ? '启动中（要等探活，可能要几十秒）…' : '提交中…';
    fetch(url, {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (r) {
      return r.json().then(function (b) { return { ok: r.ok, body: b }; });
    }).then(function (r) {
      if (!r.ok) throw new Error(r.body && r.body.error ? r.body.error : '请求失败');
      if (service) {
        if (r.body.state === 'running') {
          out.textContent = '服务已启动，正在刷新…';
          setTimeout(function () { location.reload(); }, 600);
        } else {
          out.textContent = '服务未能运行：' + (r.body.error || r.body.state || '未知原因');
        }
        return;
      }
      out.innerHTML = '已提交任务 <code>' + r.body.jobId + '</code>，网关的队列会立即开始执行。' +
        '到 <a href="/tasks">任务</a> 看状态与实时日志。';
    }).catch(function (err) {
      out.textContent = '错误：' + (err.message || err);
    });
  });
})();`

// toolFormCSS styles the service lifecycle card. Injected with the markup that
// uses it, the same way the task pages and the viewer do.
const toolFormCSS = `<style>
.svc-card { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; padding: 12px 16px; margin: 14px 0 18px; background: var(--bg-panel); border: 1px solid var(--border); border-radius: var(--r-card); font-size: 13px; }
.svc-card a { color: var(--accent); text-decoration: none; }
.svc-card a:hover { text-decoration: underline; }
.svc-dot { width: 9px; height: 9px; border-radius: 50%; flex: none; background: var(--text-muted); }
.svc-dot.on { background: #1a7f37; }
.svc-dot.warn { background: #9a6700; }
.svc-dot.bad { background: var(--danger); }
.svc-stop { margin-left: auto; padding: 4px 14px; border-radius: var(--r-pill); border: 1px solid color-mix(in srgb, var(--danger) 40%, transparent); background: transparent; color: var(--danger); font-size: 12.5px; cursor: pointer; }
.svc-stop:hover { background: color-mix(in srgb, var(--danger) 10%, transparent); }
</style>`
