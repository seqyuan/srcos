package web

import (
	"fmt"
	"strings"
)

// This file renders the self-service token page (ADR-019).
//
// The shape follows the rest of the gateway: the *list* is rendered here, so a
// user can see and revoke their credentials with JavaScript disabled. Create is
// the one action that needs a script, and for a good reason: the plaintext
// exists exactly once, and handing it back through a redirect (or a query
// string) would write a credential into browser history and every proxy log on
// the way. So the form calls `POST /api/tokens` and paints the secret into the
// page, where the user copies it and it is gone.

// TokenRow is one token, formatted for display.
type TokenRow struct {
	ID       string
	Label    string
	Scopes   string
	Tools    string
	Created  string
	Expires  string
	LastUsed string
	Status   string
	// Expired drives the row's styling.
	Expired bool
}

// TokenPage renders the page. rows is the caller's own tokens; tools is what
// they may pick from (the allowlist can only narrow their own access);
// knownScopes is the scope vocabulary the gateway understands.
func TokenPage(siteTitle, username string, rows []TokenRow, tools []string, knownScopes []string) string {
	var b strings.Builder
	b.WriteString(tokensCSS)
	b.WriteString(`<main class="wrap tk">`)
	b.WriteString(`<p class="crumb"><a href="/">仪表盘</a> / 令牌</p>`)
	b.WriteString(`<h1>Agent token</h1>`)
	b.WriteString(`<p class="muted">程序（agent、MCP 客户端、脚本）用 <code>Authorization: Bearer &lt;token&gt;</code> 调用 SRCOS。
token 以**你**的身份行事：Grant 授权与配额照常生效，scope 与工具白名单只能在此基础上收窄，永远不会放宽。</p>`)

	b.WriteString(`<div class="tk-tip">`)
	b.WriteString(`<strong>明文只出现一次。</strong> 生成后立刻复制到 agent 的配置里 —— 平台只保存 SHA-256 哈希，
无法再取回；丢了就撤销重发。`)
	b.WriteString(`</div>`)

	if len(rows) == 0 {
		b.WriteString(`<p class="muted tk-empty">还没有 token。</p>`)
	} else {
		b.WriteString(`<div class="tk-card"><table class="tk-table"><thead><tr>
  <th>ID</th><th>标签</th><th>权限</th><th>工具白名单</th><th>创建</th><th>过期</th><th>最近使用</th><th>状态</th><th></th>
</tr></thead><tbody>`)
		for _, r := range rows {
			status := `<span class="tk-tag ok">有效</span>`
			if r.Expired {
				status = `<span class="tk-tag bad">已过期</span>`
			}
			fmt.Fprintf(&b, `<tr>
  <td class="mono">%s</td>
  <td>%s</td>
  <td class="mono">%s</td>
  <td class="mono">%s</td>
  <td class="mono">%s</td>
  <td class="mono">%s</td>
  <td class="mono">%s</td>
  <td>%s</td>
  <td><button type="button" class="tk-revoke" data-revoke="%s" title="撤销">撤销</button></td>
</tr>`,
				esc(r.ID), esc(orDash(r.Label)), esc(r.Scopes), esc(orDash(r.Tools)),
				esc(r.Created), esc(r.Expires), esc(r.LastUsed), status, esc(r.ID))
		}
		b.WriteString(`</tbody></table></div>`)
	}

	// Create form. Scopes and the tool allowlist are checkboxes: the vocabulary
	// is small, and a checkbox makes "what am I granting" obvious.
	b.WriteString(`<h2>新建 token</h2><div class="tk-card tk-form">`)
	b.WriteString(`<label>标签 <input type="text" id="tok-label" placeholder="annovibe / nightly-sync" maxlength="64"></label>`)
	b.WriteString(`<fieldset><legend>权限（scope）</legend>`)
	b.WriteString(`<label class="tk-check"><input type="checkbox" value="read" checked> <code>read</code> —— 目录、路径、实例状态、日志、产物</label>`)
	b.WriteString(`<label class="tk-check"><input type="checkbox" value="submit"> <code>submit</code> —— 提交运行 / 取消 / 跑流程（蕴含 <code>read</code>）</label>`)
	b.WriteString(`</fieldset>`)

	b.WriteString(`<fieldset id="tok-tools-field"><legend>工具白名单（可选，仅对 submit 生效）</legend>`)
	if len(tools) == 0 {
		b.WriteString(`<p class="muted">你当前没有可用的工具，所以白名单为空。</p>`)
	} else {
		b.WriteString(`<p class="muted">不勾选 = 你可见的全部工具；勾选 = 只能驱动这些工具。</p><div class="tk-checks">`)
		for _, id := range tools {
			fmt.Fprintf(&b, `<label class="tk-check"><input type="checkbox" class="tok-tool" value="%s"> <code>%s</code></label>`, esc(id), esc(id))
		}
		b.WriteString(`</div>`)
	}
	b.WriteString(`</fieldset>`)

	b.WriteString(`<label>有效期 <input type="text" id="tok-expires" value="90d" list="tok-expiry-options">
  <datalist id="tok-expiry-options"><option value="30d"><option value="90d"><option value="1y"><option value="never"></datalist>
</label>`)
	b.WriteString(`<div class="tk-actions"><button type="button" id="tok-create" class="tk-primary">生成</button></div>`)
	b.WriteString(`<div id="tok-result" class="tk-result"></div>`)
	b.WriteString(`</div>`)
	b.WriteString(`</main>`)

	boot := marshalScriptJSON(map[string]any{"user": username, "scopes": knownScopes})
	return PageShell(siteTitle, "令牌", b.String()+
		`<script>window.__SRCOS_TOKENS__ = `+boot+`;</script><script>`+tokenScript+`</script>`)
}

// orDash renders an empty field as "—" rather than a blank cell.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

// tokenScript is the create/revoke client. It is deliberately small and does
// exactly two things: post the form, and show the plaintext once.
const tokenScript = `
(function () {
  var boot = window.__SRCOS_TOKENS__ || {};
  var result = document.getElementById('tok-result');
  var createBtn = document.getElementById('tok-create');

  function show(html, cls) {
    if (!result) return;
    result.innerHTML = html;
    result.className = 'tk-result' + (cls ? ' ' + cls : '');
  }

  function post(url, method, body) {
    return fetch(url, {
      method: method,
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body)
    }).then(function (r) {
      return r.json().then(function (b) { return { ok: r.ok, body: b, status: r.status }; });
    });
  }

  if (createBtn) {
    createBtn.addEventListener('click', function () {
      var scopes = [];
      var checks = document.querySelectorAll('input[type=checkbox][value=read], input[type=checkbox][value=submit]');
      for (var i = 0; i < checks.length; i++) if (checks[i].checked) scopes.push(checks[i].value);
      var tools = [];
      var toolBoxes = document.querySelectorAll('.tok-tool');
      for (var j = 0; j < toolBoxes.length; j++) if (toolBoxes[j].checked) tools.push(toolBoxes[j].value);

      var body = {
        label: (document.getElementById('tok-label') || {}).value || '',
        scopes: scopes,
        tools: tools,
        expires: (document.getElementById('tok-expires') || {}).value || '90d'
      };
      createBtn.disabled = true;
      post('/api/tokens', 'POST', body).then(function (r) {
        createBtn.disabled = false;
        if (!r.ok) {
          show('<strong>失败：</strong>' + ((r.body && r.body.error) || r.status), 'bad');
          return;
        }
        // The plaintext exists only here. Never put it in the URL.
        show('<strong>已生成 —— 明文只显示这一次，请立刻复制：</strong>' +
          '<div class="tk-secret"><code id="tok-plain">' + r.body.plaintext + '</code>' +
          '<button type="button" id="tok-copy">复制</button></div>' +
          '<div class="muted">用法：<code>Authorization: Bearer &lt;token&gt;</code>；MCP endpoint 是 <code>/mcp</code>。</div>' +
          '<div class="muted">刷新本页后列表会显示它（含过期与最近使用）。</div>', 'ok');
        var copy = document.getElementById('tok-copy');
        if (copy) copy.addEventListener('click', function () {
          var text = document.getElementById('tok-plain').textContent;
          if (navigator.clipboard) navigator.clipboard.writeText(text).then(function () { copy.textContent = '已复制'; });
        });
      }).catch(function (e) { createBtn.disabled = false; show('<strong>失败：</strong>' + e, 'bad'); });
    });
  }

  // Delegated: rows may be rendered server-side and never re-created.
  document.addEventListener('click', function (ev) {
    var btn = ev.target.closest ? ev.target.closest('[data-revoke]') : null;
    if (!btn) return;
    var id = btn.getAttribute('data-revoke');
    if (!window.confirm('撤销 token ' + id + '？使用它的程序会立即失效。')) return;
    btn.disabled = true;
    post('/api/tokens/' + encodeURIComponent(id), 'DELETE').then(function (r) {
      if (!r.ok) { btn.disabled = false; show('<strong>撤销失败：</strong>' + ((r.body && r.body.error) || r.status), 'bad'); return; }
      var row = btn.closest('tr');
      if (row) row.parentNode.removeChild(row);
      show('已撤销 ' + id + '（立即生效，网关无需重启）。', 'ok');
    }).catch(function (e) { btn.disabled = false; show('<strong>撤销失败：</strong>' + e, 'bad'); });
  });
})();`

// tokensCSS styles the token page. Injected with the markup that uses it.
const tokensCSS = `<style>
.tk h1 { font-size: 20px; margin: 0 0 6px; }
.tk h2 { font-size: 15px; margin: 26px 0 8px; }
.tk .crumb { font-size: 13px; }
.tk .crumb a { color: var(--accent); text-decoration: none; }
.tk .crumb a:hover { text-decoration: underline; }
.tk-tip { background: var(--accent-bg); border: 1px solid var(--border); border-radius: var(--r-card);
          padding: 12px 16px; font-size: 13px; color: var(--text); margin: 14px 0 18px; }
.tk-card { background: var(--bg-panel); border: 1px solid var(--border); border-radius: var(--r-card);
           overflow: hidden; box-shadow: var(--sh-card); }
.tk-table { width: 100%; border-collapse: collapse; font-size: 13px; }
.tk-table th { text-align: left; font-weight: 600; color: var(--text-muted); font-size: 11.5px;
               text-transform: uppercase; letter-spacing: .04em; padding: 10px 14px;
               border-bottom: 1px solid var(--border); white-space: nowrap; }
.tk-table td { padding: 9px 14px; border-bottom: 1px solid var(--border); vertical-align: middle; }
.tk-table tr:last-child td { border-bottom: none; }
.tk-table tbody tr:hover td { background: var(--hover); }
.mono { font-family: var(--mono); font-size: 12px; }
.tk-tag { display: inline-block; padding: 2px 9px; border-radius: var(--r-pill); font-size: 11.5px;
          font-weight: 600; border: 1px solid var(--border); color: var(--text-muted); white-space: nowrap; }
.tk-tag.ok { color: #1a7f37; border-color: color-mix(in srgb, #1a7f37 35%, transparent); background: color-mix(in srgb, #1a7f37 10%, transparent); }
.tk-tag.bad { color: var(--danger); border-color: color-mix(in srgb, var(--danger) 35%, transparent); background: color-mix(in srgb, var(--danger) 10%, transparent); }
.tk-revoke { border: 1px solid var(--border); background: transparent; color: var(--danger); font-size: 12px;
             border-radius: var(--r-pill); padding: 3px 12px; cursor: pointer; }
.tk-revoke:hover { background: var(--hover); }
.tk-form { padding: 16px 18px; display: grid; gap: 12px; }
.tk-form label { font-size: 13px; color: var(--text-muted); display: grid; gap: 5px; }
.tk-form input[type=text] { height: 34px; padding: 0 10px; border-radius: var(--r-chip);
                            border: 1px solid var(--border); background: var(--bg); color: var(--text); font-size: 13px; }
.tk-form fieldset { border: 1px solid var(--border); border-radius: var(--r-chip); padding: 10px 14px; margin: 0; }
.tk-form legend { font-size: 12px; color: var(--text-muted); padding: 0 6px; }
.tk-check { display: flex !important; align-items: center; gap: 8px; color: var(--text) !important;
            font-size: 13px; margin: 3px 0; }
.tk-checks { display: flex; flex-wrap: wrap; gap: 4px 18px; }
.tk-actions { display: flex; gap: 10px; }
.tk-primary { height: 36px; padding: 0 20px; border-radius: var(--r-pill); border: none;
              background: var(--accent); color: #fff; font-size: 13px; font-weight: 600; cursor: pointer; }
.tk-primary:hover { background: var(--accent-hover); }
.tk-primary:disabled { opacity: .6; cursor: default; }
.tk-result { font-size: 13px; }
.tk-result.ok { color: var(--text); }
.tk-result.bad { color: var(--danger); }
.tk-secret { display: flex; gap: 10px; align-items: center; margin: 8px 0; flex-wrap: wrap; }
.tk-secret code { background: var(--bg); border: 1px solid var(--border); border-radius: var(--r-chip);
                  padding: 8px 10px; font-family: var(--mono); font-size: 12.5px; word-break: break-all; }
.tk-secret button { border: 1px solid var(--border); background: transparent; color: var(--text);
                    border-radius: var(--r-pill); padding: 4px 14px; font-size: 12px; cursor: pointer; }
.tk-empty { margin-top: 14px; }
</style>`
