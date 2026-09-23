package web

import (
	"fmt"
	"strings"

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
func ToolsPage(siteTitle, username string, tools []*tool.Tool) string {
	if len(tools) == 0 {
		return PageShell(siteTitle, "工具", `<main class="wrap">
  <h1>工具</h1>
  <p class="muted">当前没有已注册且对你授权的工具。</p>
  <p class="muted">管理员可以查看 <code>docs/tool-spec.md</code> 了解如何注册一个工具。</p>
</main>`)
	}

	var b strings.Builder
	b.WriteString(`<main class="wrap"><h1>工具</h1><div class="cards">`)
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
	b.WriteString(`</div></main>`)
	return PageShell(siteTitle, "工具", b.String())
}

// ToolFormPage renders a form for one tool, built entirely from its interface.
func ToolFormPage(siteTitle, username string, t *tool.Tool, storages []storage.Storage) string {
	var b strings.Builder
	b.WriteString(`<main class="wrap">`)
	fmt.Fprintf(&b, `<p class="crumb"><a href="/tools">工具</a> / %s</p>`, esc(t.Name))
	fmt.Fprintf(&b, `<h1>%s <span class="badge">%s</span></h1>`, esc(t.Name), esc(string(t.Kind)))
	if t.Description != "" {
		fmt.Fprintf(&b, `<p class="muted">%s</p>`, esc(t.Description))
	}

	fmt.Fprintf(&b, `<form id="tool-form" data-tool="%s" data-kind="%s">`, esc(t.ID), esc(string(t.Kind)))

	if len(t.Interface.Inputs) == 0 {
		b.WriteString(`<p class="muted">该工具没有输入参数，点「运行」即可。</p>`)
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

	fmt.Fprintf(&b, `<label>任务名 <input type="text" name="__name" placeholder="%s"></label>`, esc(t.Name))
	b.WriteString(`<div class="actions"><button type="submit">运行</button></div></form>
<div id="result" class="muted"></div>
</main>`)

	return PageShell(siteTitle, t.Name, b.String()+
		`<script src="/assets/srcos-path-picker.js"></script><script>`+toolFormScript+`</script>`)
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

// toolFormScript collects the form into a job submission.
//
// It deliberately sends the interface-declared inputs separately from the
// resource overrides, mirroring the job.json shape: a tool's parameters and the
// resources it runs with are different kinds of thing, and only the resources
// are clamped by the server.
var toolFormScript = `
(function () {
  var form = document.getElementById('tool-form');
  var out = document.getElementById('result');
  if (!form) return;
  var toolID = form.dataset.tool;

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

    var body = {
      tool: toolID,
      name: fd.get('__name') || '',
      params: params
    };
    if (Object.keys(res).length) body.resources = res;

    out.textContent = '提交中…';
    fetch('/api/jobs', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (r) {
      return r.json().then(function (b) { return { ok: r.ok, body: b }; });
    }).then(function (r) {
      if (!r.ok) throw new Error(r.body && r.body.error ? r.body.error : '提交失败');
      out.innerHTML = '已提交任务 <code>' + r.body.jobId + '</code>，网关的队列会立即开始执行。' +
        '到 <a href="/tasks">任务</a> 看状态与实时日志。';
    }).catch(function (err) {
      out.textContent = '错误：' + (err.message || err);
    });
  });
})();`
