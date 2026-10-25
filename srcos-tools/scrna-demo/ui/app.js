// The tool's page. It owns the form and the presentation; SRCOS owns what
// happens after submit.
//
// The whole hand-off is five endpoints:
//   POST /api/jobs                     submit
//   GET  /api/jobs/<id>                state (the source of truth)
//   GET  /api/jobs/<id>/logs?follow=1  log stream (SSE)
//   GET  /api/jobs/<id>/artifacts      declared outputs
//   POST /api/jobs/<id>/cancel         idempotent stop
//
// Authentication is the browser's same-origin session cookie, so every URL is
// gateway-rooted (the page itself is served under /proxy/<user>/<ui-tool>/).

const TERMINAL = new Set(["succeeded", "failed", "stopped"]);
const POLL_MS = 1000;
// The one component whose input comes from the platform primitive control.
// The picker derives its browsable range from this *task* tool's interface.
const PATH_TOOL = "scrna-demo-path";

const el = {
  tool: document.getElementById("task-tool"),
  sample: document.getElementById("sample-id"),
  copies: document.getElementById("copies"),
  prefix: document.getElementById("prefix"),
  plainFields: document.getElementById("plain-fields"),
  pathField: document.getElementById("path-field"),
  picker: document.querySelector("srcos-path-picker"),
  run: document.getElementById("run"),
  retry: document.getElementById("retry"),
  cancel: document.getElementById("cancel"),
  note: document.getElementById("submit-note"),
  state: document.getElementById("state"),
  meta: document.getElementById("meta"),
  error: document.getElementById("error"),
  log: document.getElementById("log"),
  statusURL: document.getElementById("status-url"),
  artifacts: document.getElementById("artifacts"),
  artifactNote: document.getElementById("artifact-note"),
};

let current = null;      // { instanceId, jobId }
let stream = null;       // EventSource
let pollTimer = null;
let pending = null;      // last click, kept so a retry reuses the idempotency key

function setState(state, extra = "") {
  el.state.textContent = state || "未知";
  el.state.className = "badge " + (state || "");
  el.meta.textContent = extra;
}

function setBusy(busy) {
  el.run.disabled = busy;
  el.cancel.disabled = busy || !current;
}

function resetView() {
  el.log.textContent = "";
  el.error.textContent = "";
  el.artifacts.replaceChildren();
  el.artifactNote.textContent = "任务结束后从 /artifacts 读取。";
}

async function api(url, options = {}) {
  const res = await fetch(url, {
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  const text = await res.text();
  let body = null;
  try { body = text ? JSON.parse(text) : null; } catch (_) { body = { error: text }; }
  if (!res.ok) {
    const err = new Error((body && body.error) || `HTTP ${res.status}`);
    err.status = res.status;
    throw err;
  }
  return body;
}

function pickerValue() {
  if (!el.picker) return "";
  // The custom element exposes `value` once upgraded; fall back to the
  // attribute if its script has not run (the page should still be usable).
  const v = el.picker.value;
  return String(v == null ? el.picker.getAttribute("value") || "" : v).trim();
}

function clickParams() {
  const tool = el.tool.value;
  const prefix = el.prefix.value.trim() || "scrna";
  if (tool === PATH_TOOL) {
    return {
      tool,
      name: `path ${pickerValue() || "未选择"}`,
      params: { input_dir: pickerValue(), prefix },
      outputs: ["/workspace/out"],
      tags: { ui: "scrna-demo", input: "path" },
    };
  }
  return {
    tool,
    name: `scrna ${el.sample.value.trim() || "S001"}`,
    params: {
      sample_id: el.sample.value.trim(),
      copies: Number(el.copies.value),
      prefix,
    },
    // The parent directory: each run writes its own child keyed by task id.
    outputs: ["/workspace/out"],
    tags: { ui: "scrna-demo" },
  };
}

function syncToolFields() {
  const isPath = el.tool.value === PATH_TOOL;
  el.plainFields.hidden = isPath;
  el.pathField.hidden = !isPath;
  el.note.textContent = isPath
    ? "选一个已存在的目录；路径范围来自 task 工具的 storage 闭包。"
    : "每次点击生成一个新的幂等键；重复提交同一次点击不会产生第二个任务。";
}

async function submit(payload, idempotencyKey) {
  setBusy(true);
  el.error.textContent = "";
  try {
    const body = { ...payload, idempotencyKey };
    const res = await api("/api/jobs", { method: "POST", body: JSON.stringify(body) });
    const instanceId = res.instanceId;
    if (!instanceId) throw new Error("submit response has no instanceId");
    pending = null;
    el.retry.hidden = true;
    current = { instanceId, jobId: res.jobId };
    resetView();
    el.statusURL.textContent = `/api/jobs/${instanceId}`;
    el.note.textContent = `已提交：${instanceId}`;
    // Open the stream first: a tiny task can be terminal by the first poll, and
    // the SSE replay is what shows its log. The stream closes on `end`; the
    // poll only closes it on error, so a fast completion still gets its tail.
    openStream();
    await refresh();
  } catch (err) {
    el.error.textContent = `${err.status ? `[${err.status}] ` : ""}${err.message}`;
    el.note.textContent = "提交失败；可用「重试提交」复用同一个幂等键。";
    el.retry.hidden = false;
    setBusy(false);
  }
}

async function refresh() {
  if (!current) return;
  try {
    const view = await api(`/api/jobs/${encodeURIComponent(current.instanceId)}`);
    const extra = [];
    if (view.tool) extra.push(view.tool);
    if (typeof view.exitCode === "number" && view.exitCode !== 0) extra.push(`exit ${view.exitCode}`);
    if (view.duration) extra.push(view.duration);
    if (view.startedAt) extra.push(view.startedAt);
    setState(view.state, extra.join(" · "));
    el.error.textContent = view.error || "";
    el.cancel.disabled = TERMINAL.has(view.state);
    if (TERMINAL.has(view.state)) {
      stopPolling();
      // Do not close the stream here: it still has the tail to replay, and its
      // own `end` event is the signal that everything was delivered.
      await loadArtifacts();
      setBusy(false);
      return;
    }
    schedulePoll();
  } catch (err) {
    el.error.textContent = `${err.status ? `[${err.status}] ` : ""}${err.message}`;
    schedulePoll();
  }
}

function schedulePoll() {
  stopPolling();
  pollTimer = setTimeout(refresh, POLL_MS);
}

function stopPolling() {
  if (pollTimer) clearTimeout(pollTimer);
  pollTimer = null;
}

function openStream() {
  closeStream();
  if (!current) return;
  stream = new EventSource(`/api/jobs/${encodeURIComponent(current.instanceId)}/logs?follow=1`);
  stream.addEventListener("line", (ev) => appendLog(ev.data + "\n"));
  stream.addEventListener("note", (ev) => appendLog(`[note] ${ev.data}\n`));
  // The end event's state is a hint (the process is gone); the status endpoint
  // remains authoritative, so ask it once more instead of trusting the stream.
  stream.addEventListener("state", () => refresh());
  stream.addEventListener("end", () => closeStream());
  stream.onerror = () => {
    // A closed stream after the end event is normal. If it drops early, the
    // status poll still converges, so do not turn this into a page error.
    closeStream();
  };
}

function closeStream() {
  if (stream) stream.close();
  stream = null;
}

function appendLog(text) {
  const atBottom = el.log.scrollTop + el.log.clientHeight >= el.log.scrollHeight - 4;
  el.log.textContent += text;
  if (atBottom) el.log.scrollTop = el.log.scrollHeight;
}

async function loadArtifacts() {
  if (!current) return;
  try {
    const res = await api(`/api/jobs/${encodeURIComponent(current.instanceId)}/artifacts`);
    const items = (res && res.artifacts) || [];
    el.artifactNote.textContent = items.length ? `${items.length} 个声明产物` : "该任务没有声明产物";
    el.artifacts.replaceChildren();
    for (const a of items) {
      const li = document.createElement("li");
      const name = document.createElement("code");
      name.textContent = a.path;
      li.append(name);
      if (a.addr) {
        const link = document.createElement("a");
        // /view is the platform's viewer; the tool only passes the address.
        link.href = `/view?src=${encodeURIComponent(a.addr)}`;
        link.textContent = " 打开";
        link.target = "_blank";
        li.append(link);
      }
      const note = document.createElement("div");
      note.className = "muted";
      const bits = [];
      if (a.exists === false) bits.push(a.note || "missing");
      if (a.isDir) bits.push(`目录，${a.entries ?? 0} 项`);
      else if (a.size) bits.push(`${a.size} B`);
      if (a.mtime) bits.push(a.mtime);
      note.textContent = bits.join(" · ");
      li.append(note);
      el.artifacts.append(li);
    }
  } catch (err) {
    el.artifactNote.textContent = `读取产物失败：${err.message}`;
  }
}

el.tool.addEventListener("change", syncToolFields);
syncToolFields();

el.run.addEventListener("click", () => {
  const payload = clickParams();
  if (!payload.tool) { el.error.textContent = "任务工具不能为空"; return; }
  if (payload.tool === PATH_TOOL && !payload.params.input_dir) {
    el.error.textContent = "请先选择一个数据目录（或确认 data storage 已声明）";
    return;
  }
  if (payload.tool !== PATH_TOOL && !payload.params.sample_id) {
    el.error.textContent = "样本 ID 不能为空";
    return;
  }
  const key = (crypto.randomUUID && crypto.randomUUID()) ||
    `ui-${Date.now()}-${Math.random().toString(36).slice(2)}`;
  pending = { payload, key };
  submit(payload, key);
});

el.retry.addEventListener("click", () => {
  if (pending) submit(pending.payload, pending.key);
});

el.cancel.addEventListener("click", async () => {
  if (!current) return;
  el.cancel.disabled = true;
  try {
    await api(`/api/jobs/${encodeURIComponent(current.instanceId)}/cancel`,
      { method: "POST", body: "{}" });
    await refresh();
  } catch (err) {
    el.error.textContent = `${err.status ? `[${err.status}] ` : ""}${err.message}`;
    el.cancel.disabled = false;
  }
});
