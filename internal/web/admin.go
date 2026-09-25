package web

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/seqyuan/srcos/internal/accessrequest"
	"github.com/seqyuan/srcos/internal/grant"
	"github.com/seqyuan/srcos/internal/inspect"
	"github.com/seqyuan/srcos/internal/tool"
)

// This file renders the management console. It is server-rendered on purpose:
// the tables are the point, and an operator staring at a blank page because a
// fetch failed learns nothing. JavaScript is used for the *actions* (stop an
// instance, edit a grant), which is where it earns its place.

// AdminData is everything the console shows.
type AdminData struct {
	Instances []inspect.AdminInstance
	Tools     []inspect.AdminTool
	Admins    []string
	Groups    map[string][]string
	// Requests is every *pending* access request (B3); decided ones are history
	// the operator reads through `srcos audit` / the audit page.
	Requests []accessrequest.Request
	// Users is every registered account, for the "start a service as this
	// user" form (the console can already stop, so it can start too).
	Users []string
	// DefaultAllow is shown because it silently overrides every grant.
	DefaultAllow bool
}

// AdminPage renders the management console.
func AdminPage(siteTitle, username string, data AdminData) string {
	defaultNote := ""
	if data.DefaultAllow {
		defaultNote = " <strong>注意：default_allow: true —— 所有登录用户都能看到全部工具。</strong>"
	}
	body := fmt.Sprintf(adminTpl,
		esc(username),
		adminRequestsTable(data.Requests),
		adminInstancesTable(data.Instances),
		adminStartServiceForm(data),
		defaultNote,
		adminToolsTable(data.Tools),
		adminUsersTable(data),
	)
	// The action layer runs after the tables, so every element it looks up
	// exists when it runs.
	body += `<script>var ADMIN_STATE = ` + AdminStateJSON(data) + `;</script>` +
		`<script>` + adminScript + `</script>`
	return PageShell(siteTitle, "管理控制台", body)
}

// adminStartServiceForm renders the "start a service as a user" control.
//
// The console could stop an instance but not start one; this closes the gap
// without leaving the browser. Both lists are rendered server-side, so the note
// can say precisely what is missing when there is nothing to start.
func adminStartServiceForm(data AdminData) string {
	var services []inspect.AdminTool
	for _, t := range data.Tools {
		if t.Kind == string(tool.KindService) {
			services = append(services, t)
		}
	}
	if len(data.Users) == 0 || len(services) == 0 {
		return `<div class="adm-empty">启动服务需要：至少一个注册用户，以及一个 <span class="mono">kind: service</span> 的工具包。</div>`
	}

	var b strings.Builder
	b.WriteString(`<form class="adm-start">`)
	b.WriteString(`<select name="user">`)
	for _, u := range data.Users {
		b.WriteString(`<option value="` + esc(u) + `">` + esc(u) + `</option>`)
	}
	b.WriteString(`</select>`)
	b.WriteString(`<select name="tool">`)
	for _, t := range services {
		b.WriteString(`<option value="` + esc(t.ID) + `">` + esc(t.ID) + ` ` + esc(t.Version) + `</option>`)
	}
	b.WriteString(`</select>`)
	b.WriteString(`<button type="button" class="adm-btn primary" onclick="admStartService(this)">启动服务</button>`)
	b.WriteString(`<span class="adm-hint-inline">以该用户身份启动一个实例（等价于 <span class="mono">srcos svc start --user</span>）；已存在则先停后起。</span>`)
	b.WriteString(`</form>`)
	return b.String()
}

// adminRequestsTable renders the pending access requests (B3): the one table an
// operator actually has to act on, so it sits at the top of the console.
func adminRequestsTable(reqs []accessrequest.Request) string {
	if len(reqs) == 0 {
		return `<div class="adm-empty">没有待审的申请。用户在 <code>/tools</code> 页对「可申请」的工具提交后，会出现在这里。</div>`
	}
	var b strings.Builder
	b.WriteString(`<table class="adm"><thead><tr><th>提交时间</th><th>用户</th><th>工具</th><th>用途</th><th></th></tr></thead><tbody>`)
	for _, r := range reqs {
		fmt.Fprintf(&b, `<tr>
      <td class="adm-note">%s</td>
      <td>%s</td>
      <td><code>%s</code></td>
      <td>%s</td>
      <td class="right">
        <button type="button" class="adm-btn primary sm" onclick="admApproveRequest('%s')">批准</button>
        <button type="button" class="adm-btn sm danger" onclick="admDenyRequest('%s')">拒绝</button>
      </td>
    </tr>`,
			esc(r.CreatedAt.Local().Format("2006-01-02 15:04")), esc(r.User), esc(r.Tool),
			esc(r.Reason), esc(r.ID), esc(r.ID))
	}
	b.WriteString(`</tbody></table>`)
	return b.String()
}

// adminInstancesTable renders the instance overview.
func adminInstancesTable(instances []inspect.AdminInstance) string {
	if len(instances) == 0 {
		return `<div class="adm-empty">还没有任何实例记录。</div>`
	}
	var b strings.Builder
	b.WriteString(`<table class="adm"><thead><tr>`)
	for _, h := range []string{"用户", "实例", "工具", "状态", "端点 / 路由", "启动于", "资源", "占用（快照）", ""} {
		b.WriteString("<th>" + h + "</th>")
	}
	b.WriteString(`</tr></thead><tbody>`)

	for _, v := range instances {
		inst := v.Inst
		usage := `<span class="muted">-</span>`
		if v.Usage != nil {
			usage = fmt.Sprintf(`<span class="mono">%s</span><br><span class="adm-note">CPU %.1fs</span>`,
				esc(v.Usage.RSSHuman), v.Usage.CPUSeconds)
		}
		requested := `<span class="muted">-</span>`
		if inst.Endpoint != "" || inst.RoutePath != "" {
			requested = fmt.Sprintf(`<span class="mono">%s</span><br><span class="adm-note">%s</span>`,
				esc(inst.Endpoint), esc(inst.RoutePath))
		}
		started := `<span class="muted">-</span>`
		if inst.StartedAt != "" {
			started = esc(shortTime(inst.StartedAt))
			if inst.Duration != "" {
				started += fmt.Sprintf(`<br><span class="adm-note">%s</span>`, esc(inst.Duration))
			}
		}
		errLine := ""
		if inst.Error != "" {
			errLine = fmt.Sprintf(`<br><span class="adm-note">%s</span>`, esc(inst.Error))
		}

		action := `<span class="muted">-</span>`
		if !instanceTerminal(inst.State) {
			action = fmt.Sprintf(`<button type="button" class="adm-btn sm danger" onclick="admStop('%s')">强制停止</button>
        <button type="button" class="adm-btn sm" onclick="admLogs('%s')">日志</button>`, esc(inst.ID), esc(inst.ID))
		} else {
			action = fmt.Sprintf(`<button type="button" class="adm-btn sm" onclick="admLogs('%s')">日志</button>`, esc(inst.ID))
		}

		fmt.Fprintf(&b, `<tr>
      <td>%s</td>
      <td><span class="mono">%s</span><br><span class="adm-note">%s</span></td>
      <td>%s<br><span class="adm-note">%s · %s</span></td>
      <td><span class="tag %s">%s</span>%s</td>
      <td>%s</td>
      <td>%s</td>
      <td>%s</td>
      <td>%s</td>
      <td class="right">%s<div id="logs-%s"></div></td>
    </tr>`,
			esc(v.Owner), esc(inst.ID), esc(inst.Name),
			esc(inst.Tool), esc(inst.Kind), esc(inst.Backend),
			stateClass(inst.State), esc(inst.State), errLine,
			requested, started, usage, resourcesCell(inst),
			action, esc(inst.ID))
	}
	b.WriteString(`</tbody></table>`)
	return b.String()
}

// adminToolsTable renders the catalogue with an inline grant editor per tool.
func adminToolsTable(tools []inspect.AdminTool) string {
	if len(tools) == 0 {
		return `<div class="adm-empty">本机没有找到工具包（检查 --tools-dir）。</div>`
	}
	var b strings.Builder
	b.WriteString(`<table class="adm"><thead><tr><th>工具</th><th>类型</th><th>谁能用</th><th>配额（该用户+该工具）</th><th></th></tr></thead><tbody>`)

	for _, t := range tools {
		who := `<span class="tag warn">未授权</span> <span class="adm-note">非管理员不可见</span>`
		if t.Grant != nil {
			who = grantSummary(*t.Grant)
		}
		// A requestable tool is not granted, but its existence is public so
		// users can ask for it (B3).
		if t.Grant != nil && t.Grant.Requestable {
			who += ` <span class="tag">可申请</span>`
		}
		quota := `<span class="muted">-</span>`
		if t.Grant != nil && !t.Grant.Quota.IsZero() {
			quota = fmt.Sprintf("cpu %d · mem %s · 实例 %d",
				t.Grant.Quota.MaxCPU, dashStr(t.Grant.Quota.MaxMemory), t.Grant.Quota.MaxInstances)
		}
		fmt.Fprintf(&b, `<tr>
      <td><strong>%s</strong><br><span class="mono adm-note">%s</span></td>
      <td>%s<br><span class="adm-note">%s</span></td>
      <td>%s</td>
      <td class="adm-note">%s</td>
      <td class="right"><details class="grant"><summary>编辑授权</summary>%s</details></td>
    </tr>`,
			esc(t.Name), esc(t.ID), esc(t.Kind), esc(t.Backend), who, quota, grantForm(t))
	}
	b.WriteString(`</tbody></table>`)
	return b.String()
}

// grantForm renders the editor for one tool's grant.
//
// It is a plain form: the values are read by the small script below, sent as
// JSON to /api/admin/grants/<tool>, and the page reloads. No client-side state
// machine, nothing to get out of sync with the policy on disk.
func grantForm(t inspect.AdminTool) string {
	users, groups, public, requestable := "", "", false, false
	maxCPU, maxMemory, maxInstances := 0, "", 0
	if t.Grant != nil {
		users = strings.Join(t.Grant.Users, ", ")
		groups = strings.Join(t.Grant.Groups, ", ")
		public = t.Grant.Public
		requestable = t.Grant.Requestable
		maxCPU = t.Grant.Quota.MaxCPU
		maxMemory = t.Grant.Quota.MaxMemory
		maxInstances = t.Grant.Quota.MaxInstances
	}
	checked, reqChecked := "", ""
	if public {
		checked = " checked"
	}
	if requestable {
		reqChecked = " checked"
	}
	return fmt.Sprintf(`<div class="grant-form" data-tool="%s">
      <label>用户（逗号分隔）<input type="text" name="users" value="%s" placeholder="alice, bob"></label>
      <label>组（逗号分隔，需已存在）<input type="text" name="groups" value="%s" placeholder="bio-team"></label>
      <label class="grant-check"><input type="checkbox" name="public"%s> 所有登录用户可用（public）</label>
      <label class="grant-check"><input type="checkbox" name="requestable"%s> 未授权用户可申请（requestable）—— 只公开存在性，不授予权限</label>
      <div class="grant-row">
        <label>最大核数<input type="number" name="maxCpu" value="%d" min="0"></label>
        <label>最大内存<input type="text" name="maxMemory" value="%s" placeholder="64Gi"></label>
        <label>最大实例<input type="number" name="maxInstances" value="%d" min="0"></label>
      </div>
      <div class="adm-actions">
        <button type="button" class="adm-btn primary sm" onclick="admSaveGrant(this)">保存</button>
        <button type="button" class="adm-btn sm danger" onclick="admRemoveGrant('%s')">删除授权（等于下架）</button>
      </div>
    </div>`, esc(t.ID), esc(users), esc(groups), checked, reqChecked, maxCPU, esc(maxMemory), maxInstances, esc(t.ID))
}

// adminUsersTable renders the groups and admins, with an editor that replaces
// the whole list (the policy's own semantics: a group is a list, not a delta).
func adminUsersTable(data AdminData) string {
	var b strings.Builder
	b.WriteString(`<table class="adm"><thead><tr><th>组</th><th>成员</th><th></th></tr></thead><tbody>`)

	names := make([]string, 0, len(data.Groups))
	for name := range data.Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, `<tr>
      <td><strong>%s</strong></td>
      <td class="mono">%s</td>
      <td class="right"><details class="grant"><summary>编辑成员</summary>
        <div class="grant-form" data-group="%s">
          <label>成员（逗号分隔；清空 = 删除该组）<input type="text" name="users" value="%s"></label>
          <div class="adm-actions"><button type="button" class="adm-btn primary sm" onclick="admSaveGroup(this)">保存</button></div>
        </div>
      </details></td>
    </tr>`, esc(name), esc(strings.Join(data.Groups[name], ", ")), esc(name), esc(strings.Join(data.Groups[name], ", ")))
	}
	if len(names) == 0 {
		b.WriteString(`<tr><td colspan="3" class="adm-empty">还没有组。</td></tr>`)
	}
	b.WriteString(`</tbody></table>`)

	b.WriteString(`<table class="adm"><thead><tr><th>管理员</th><th></th></tr></thead><tbody>`)
	if len(data.Admins) == 0 {
		b.WriteString(`<tr><td colspan="2" class="adm-empty">没有管理员（应当手动编辑 grants.yaml 修复）。</td></tr>`)
	}
	for _, a := range data.Admins {
		fmt.Fprintf(&b, `<tr><td class="mono">%s</td><td class="right"><button type="button" class="adm-btn sm danger" onclick="admRemoveAdmin('%s')">移除</button></td></tr>`,
			esc(a), esc(a))
	}
	b.WriteString(`<tr><td colspan="2">
    <div class="grant-form" data-admin-add="1">
      <label>新增管理员（用户名）<input type="text" name="user" placeholder="alice"></label>
      <div class="adm-actions"><button type="button" class="adm-btn primary sm" onclick="admAddAdmin(this)">加入</button></div>
    </div>
  </td></tr>`)
	b.WriteString(`</tbody></table>`)
	return b.String()
}

// ─────────────────────────────────────────────────────────────────────────
// 小工具
// ─────────────────────────────────────────────────────────────────────────

func grantSummary(g grant.Grant) string {
	var parts []string
	if g.Public {
		parts = append(parts, `<span class="tag ok">public</span>`)
	}
	for _, u := range g.Users {
		parts = append(parts, `<span class="tag">`+esc(u)+`</span>`)
	}
	for _, grp := range g.Groups {
		parts = append(parts, `<span class="tag">组:`+esc(grp)+`</span>`)
	}
	if len(parts) == 0 {
		return `<span class="tag warn">授权给了没有人</span>`
	}
	return strings.Join(parts, " ")
}

// resourcesCell shows what the instance asked for (the ceiling the tool
// declared and the grant allowed), next to the live snapshot.
func resourcesCell(inst inspect.InstanceView) string {
	var parts []string
	if inst.Outputs != nil {
		parts = append(parts, fmt.Sprintf("%d 个声明产物", len(inst.Outputs)))
	}
	if inst.Limiter != "" {
		parts = append(parts, "限额: "+esc(inst.Limiter))
	}
	if inst.Sandbox != "" {
		parts = append(parts, "沙箱: "+esc(inst.Sandbox))
	}
	if len(parts) == 0 {
		return `<span class="muted">-</span>`
	}
	return strings.Join(parts, "<br>")
}

func stateClass(state string) string {
	switch state {
	case "running":
		return "ok"
	case "starting", "pending", "submitted", "idle":
		return "warn"
	default:
		return "bad"
	}
}

func instanceTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "stopped":
		return true
	}
	return false
}

// shortTime trims an RFC3339 timestamp to something readable in a table.
func shortTime(ts string) string {
	if len(ts) >= 16 {
		return ts[:10] + " " + ts[11:16]
	}
	return ts
}

func dashStr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// adminScript is the console's action layer.
const adminScript = `
function admToast(msg, bad) {
  var t = document.getElementById('adm-toast');
  t.textContent = msg;
  t.className = 'adm-toast show' + (bad ? ' bad' : '');
  setTimeout(function(){ t.className = 'adm-toast' + (bad ? ' bad' : ''); }, 4000);
}
function admFetch(method, url, body, okMsg) {
  return fetch(url, {
    method: method,
    credentials: 'same-origin',
    headers: body ? {'Content-Type': 'application/json'} : {},
    body: body ? JSON.stringify(body) : undefined
  }).then(function(r) {
    return r.json().catch(function(){ return {}; }).then(function(data) {
      if (!r.ok) { throw new Error(data.error || ('HTTP ' + r.status)); }
      admToast(okMsg || '已保存', false);
      setTimeout(function(){ location.reload(); }, 500);
      return data;
    });
  }).catch(function(e) { admToast(e.message, true); });
}
function admList(value) {
  return (value || '').split(',').map(function(s){ return s.trim(); }).filter(function(s){ return s.length > 0; });
}
function admSaveGrant(btn) {
  var form = btn.closest('.grant-form');
  var tool = form.getAttribute('data-tool');
  var body = {
    users: admList(form.querySelector('[name=users]').value),
    groups: admList(form.querySelector('[name=groups]').value),
    public: form.querySelector('[name=public]').checked,
    requestable: form.querySelector('[name=requestable]').checked,
    maxCpu: parseInt(form.querySelector('[name=maxCpu]').value || '0', 10) || 0,
    maxMemory: form.querySelector('[name=maxMemory]').value.trim(),
    maxInstances: parseInt(form.querySelector('[name=maxInstances]').value || '0', 10) || 0
  };
  admFetch('PUT', '/api/admin/grants/' + encodeURIComponent(tool), body, '授权已保存并立即生效');
}
function admRemoveGrant(tool) {
  if (!confirm('删除 ' + tool + ' 的授权？该工具将只对管理员可见（等于下架）。')) { return; }
  admFetch('DELETE', '/api/admin/grants/' + encodeURIComponent(tool), null, '授权已删除');
}
function admStartService(btn) {
  var form = btn.closest('.adm-start');
  var user = form.querySelector('[name=user]').value;
  var tool = form.querySelector('[name=tool]').value;
  if (!confirm('以 ' + user + ' 的身份启动 ' + tool + '？')) { return; }
  admFetch('POST', '/api/admin/instances', {user: user, tool: tool}, '服务已启动');
}
function admApproveRequest(id) {
  if (!confirm('批准 ' + id + '？将立即写入一条 grant，授权马上生效。')) { return; }
  admFetch('POST', '/api/admin/requests/' + encodeURIComponent(id) + '/approve', {}, '已批准，授权立即生效');
}
function admDenyRequest(id) {
  var note = prompt('拒绝 ' + id + ' 的理由（可留空）：');
  if (note === null) { return; }
  admFetch('POST', '/api/admin/requests/' + encodeURIComponent(id) + '/deny', {note: note}, '已拒绝');
}
function admSaveGroup(btn) {
  var form = btn.closest('.grant-form');
  var name = form.getAttribute('data-group');
  var users = admList(form.querySelector('[name=users]').value);
  if (users.length === 0 && !confirm('成员清空后会删除组 ' + name + '，确定？')) { return; }
  admFetch('PUT', '/api/admin/groups', {name: name, users: users}, '组已更新');
}
function admStop(id) {
  if (!confirm('强制停止 ' + id + '？实例所有者不会收到确认请求。')) { return; }
  admFetch('POST', '/api/admin/instances/' + encodeURIComponent(id) + '/stop', null, '已停止 ' + id);
}
function admLogs(id) {
  var box = document.getElementById('logs-' + id);
  if (!box) { return; }
  if (box.dataset.open === '1') { box.innerHTML = ''; box.dataset.open = '0'; return; }
  fetch('/api/admin/instances/' + encodeURIComponent(id) + '/logs?tail=200', {credentials: 'same-origin'})
    .then(function(r){ return r.json(); })
    .then(function(d){
      var pre = document.createElement('pre');
      pre.className = 'logs-pre';
      pre.textContent = d.log || d.error || '(空)';
      box.innerHTML = '';
      box.appendChild(pre);
      box.dataset.open = '1';
    })
    .catch(function(e){ admToast(e.message, true); });
}
function admAddAdmin(btn) {
  var form = btn.closest('.grant-form');
  var user = form.querySelector('[name=user]').value.trim();
  if (!user) { return; }
  var admins = ADMIN_STATE.admins.slice();
  if (admins.indexOf(user) === -1) { admins.push(user); }
  admFetch('PUT', '/api/admin/admins', {admins: admins}, '已加入管理员 ' + user);
}
function admRemoveAdmin(user) {
  var admins = ADMIN_STATE.admins.filter(function(a){ return a !== user; });
  if (admins.length === 0) { admToast('至少要保留一个管理员', true); return; }
  if (!confirm('移除管理员 ' + user + '？')) { return; }
  admFetch('PUT', '/api/admin/admins', {admins: admins}, '已移除 ' + user);
}
`

// AdminStateJSON is the small state the console's actions need client-side: the
// admin list (to add/remove one) and nothing else, because every other action
// reads the form it was called from.
func AdminStateJSON(data AdminData) string {
	raw, err := json.Marshal(map[string]any{"admins": data.Admins})
	if err != nil {
		return "{}"
	}
	return string(raw)
}
