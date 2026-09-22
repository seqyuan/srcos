/*
 * <srcos-path-picker> — a primitive control, not a business form.
 *
 * This is the first instance of the rule that SRCOS ships *controls* while tool
 * authors ship *UIs* (AGENTS.md: 平台只提供原语控件). It is deliberately a plain
 * Web Component with no dependencies and no build step, so all three consumers
 * can use the same element:
 *
 *   1. SRCOS's generated fallback form (web.ToolFormPage);
 *   2. a tool's own shiny / python / R page — one script tag and one element;
 *   3. anything else that renders HTML.
 *
 * Usage:
 *
 *   <script src="/assets/srcos-path-picker.js"></script>
 *   <srcos-path-picker tool="cellranger" input="ref" name="ref" required></srcos-path-picker>
 *
 * Attributes:
 *   tool, input   which tool interface input this value belongs to. Together
 *                 they are the *allowlist*: the server derives the browsable
 *                 storages from them and refuses anything else, so a page
 *                 cannot widen its own reach.
 *   name          when set, a hidden input with this name is rendered so a
 *                 plain HTML form serialises the value without any JS.
 *   value         initial value (a sandbox path, e.g. /data/ref).
 *   select        "file" | "directory"; defaults to what the interface declares.
 *   label         overrides the field label.
 *   placeholder   input placeholder.
 *
 * Events:
 *   change        detail: { value }, bubbles and is composed, so it escapes the
 *                 shadow root for a listening tool UI.
 */
(function () {
  'use strict';
  if (customElements.get('srcos-path-picker')) return;

  var CSS = `
    :host { display: block; font: inherit; }
    .row { display: flex; gap: .5rem; align-items: stretch; }
    input {
      flex: 1 1 auto; min-width: 0;
      font: inherit; padding: .45rem .6rem;
      border: 1px solid var(--border, #d5d9e0); border-radius: 6px;
      background: var(--bg-panel, #fff); color: inherit;
    }
    input:focus { outline: 2px solid var(--accent, #3b6ef5); outline-offset: -1px; }
    button {
      flex: 0 0 auto; font: inherit; cursor: pointer;
      padding: .45rem .8rem; border-radius: 6px;
      border: 1px solid var(--border, #d5d9e0);
      background: var(--bg-hover, #f4f6f9); color: inherit;
    }
    button:hover { background: var(--bg-selected, #e8ecf3); }
    .hint { margin-top: .25rem; font-size: .8em; color: var(--text-muted, #6b7280); }
    dialog {
      padding: 0; border: 1px solid var(--border, #d5d9e0); border-radius: 10px;
      width: min(640px, 92vw); max-height: 80vh;
      background: var(--bg-panel, #fff); color: inherit;
      box-shadow: 0 12px 40px rgba(0,0,0,.25);
    }
    dialog::backdrop { background: rgba(0,0,0,.45); }
    header {
      display: flex; align-items: center; gap: .75rem;
      padding: .7rem .9rem; border-bottom: 1px solid var(--border, #d5d9e0);
      font-weight: 600;
    }
    header .grow { flex: 1 1 auto; min-width: 0; }
    .crumb {
      padding: .55rem .9rem; font-size: .85em; font-family: ui-monospace, monospace;
      color: var(--text-muted, #6b7280); border-bottom: 1px solid var(--border, #d5d9e0);
      overflow-wrap: anywhere;
    }
    ul { list-style: none; margin: 0; padding: .3rem 0; overflow: auto; max-height: 52vh; }
    li {
      display: flex; align-items: center; gap: .6rem;
      padding: .4rem .9rem; cursor: pointer;
    }
    li:hover { background: var(--bg-hover, #f4f6f9); }
    li .icon { flex: 0 0 1.1em; text-align: center; opacity: .75; }
    li .name { flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    li .meta { flex: 0 0 auto; font-size: .8em; color: var(--text-dim, #9ca3af); }
    li[aria-disabled="true"] { opacity: .45; cursor: default; }
    footer {
      display: flex; align-items: center; gap: .6rem;
      padding: .7rem .9rem; border-top: 1px solid var(--border, #d5d9e0);
    }
    footer .grow { flex: 1 1 auto; }
    .status { padding: .8rem .9rem; color: var(--text-muted, #6b7280); }
    .error { padding: .8rem .9rem; color: var(--danger, #b3261e); overflow-wrap: anywhere; }
  `;

  function fmtSize(n) {
    if (!n) return '';
    var u = ['B', 'K', 'M', 'G', 'T'], i = 0;
    while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
    return (i === 0 ? n : n.toFixed(1)) + u[i];
  }

  var PathPicker = class extends HTMLElement {
    constructor() {
      super();
      this._root = this.attachShadow({ mode: 'open' });
      this._value = '';
      this._listing = null;
      this._picked = null; // the sandbox path chosen inside the dialog
    }

    static get observedAttributes() { return ['value', 'tool', 'input', 'select', 'label', 'placeholder', 'required']; }

    connectedCallback() {
      this._value = this.getAttribute('value') || '';
      this._render();
    }

    attributeChangedCallback(name, _old, next) {
      if (name === 'value' && next !== null && next !== this._value) {
        this._value = next;
        var input = this._root.querySelector('input[type=text]');
        if (input) input.value = next;
      }
    }

    get value() { return this._value; }
    set value(v) {
      this._value = v == null ? '' : String(v);
      this.setAttribute('value', this._value);
    }

    _label() {
      return this.getAttribute('label') || this.getAttribute('input') || '路径';
    }

    _emit() {
      this.dispatchEvent(new CustomEvent('change', {
        detail: { value: this._value },
        bubbles: true,
        composed: true,
      }));
    }

    _render() {
      var label = this._label();
      var placeholder = this.getAttribute('placeholder') || '/data/…';
      var required = this.hasAttribute('required') ? ' required' : '';
      var hidden = this.getAttribute('name')
        ? '<input type="hidden" name="' + esc(this.getAttribute('name')) + '">'
        : '';

      this._root.innerHTML = '<style>' + CSS + '</style>' +
        '<div class="row">' +
          '<input type="text" value="' + esc(this._value) + '" placeholder="' + esc(placeholder) + '"' +
            ' aria-label="' + esc(label) + '"' + required + '>' +
          '<button type="button" title="浏览">浏览…</button>' +
        '</div>' +
        '<div class="hint">' + esc(this.getAttribute('tool') || '') +
          (this.getAttribute('input') ? ' · ' + esc(this.getAttribute('input')) : '') + '</div>' +
        hidden +
        '<dialog></dialog>';

      var input = this._root.querySelector('input[type=text]');
      var hiddenInput = this._root.querySelector('input[type=hidden]');
      var dialog = this._root.querySelector('dialog');
      var self = this;

      input.addEventListener('input', function () {
        self._value = input.value;
        if (hiddenInput) hiddenInput.value = input.value;
        self._emit();
      });

      this._root.querySelector('button').addEventListener('click', function () {
        self._open(dialog);
      });
    }

    _open(dialog) {
      this._picked = null;
      var start = this._value || '';
      var self = this;
      dialog.showModal();
      this._browse(dialog, start);

      dialog.addEventListener('cancel', function (e) { e.preventDefault(); dialog.close(); });
      // A click on the backdrop closes: the dialog box itself does not cover it.
      dialog.addEventListener('click', function (e) {
        if (e.target === dialog) dialog.close();
      });

      dialog.addEventListener('close', function () {
        if (self._picked) {
          self.value = self._picked;
          self._emit();
        }
      }, { once: true });
    }

    _browse(dialog, path) {
      var tool = this.getAttribute('tool') || '';
      var input = this.getAttribute('input') || '';
      var sel = this.getAttribute('select') || '';
      var url = '/api/paths?tool=' + encodeURIComponent(tool) +
        '&input=' + encodeURIComponent(input) +
        (path ? '&path=' + encodeURIComponent(path) : '') +
        (sel ? '&select=' + encodeURIComponent(sel) : '');

      var self = this;
      dialog.innerHTML = '<style>' + CSS + '</style><div class="status">加载中…</div>';

      fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
        .then(function (r) {
          return r.json().then(function (body) { return { ok: r.ok, body: body }; });
        })
        .then(function (res) {
          if (!res.ok) throw new Error(res.body && res.body.error ? res.body.error : '请求失败');
          self._listing = res.body;
          self._draw(dialog);
        })
        .catch(function (err) {
          dialog.innerHTML = '<style>' + CSS + '</style>' +
            '<div class="error">' + esc(String(err.message || err)) + '</div>' +
            '<footer><span class="grow"></span><button type="button" data-act="close">关闭</button></footer>';
          var b = dialog.querySelector('[data-act=close]');
          if (b) b.addEventListener('click', function () { dialog.close(); });
        });
    }

    _draw(dialog) {
      var d = this._listing;
      var self = this;
      var selectIsDir = d.select !== 'file';

      var items = d.entries.map(function (e) {
        var selectable = selectIsDir ? e.isDir : !e.isDir;
        return '<li data-path="' + esc(e.path) + '" data-dir="' + (e.isDir ? '1' : '0') + '"' +
          ' aria-disabled="' + (selectable ? 'false' : 'true') + '">' +
          '<span class="icon">' + (e.isDir ? '📁' : '📄') + '</span>' +
          '<span class="name">' + esc(e.name) + '</span>' +
          '<span class="meta">' + (e.isDir ? '' : fmtSize(e.size)) + '</span>' +
          '</li>';
      }).join('');

      dialog.innerHTML = '<style>' + CSS + '</style>' +
        '<header><span class="grow">选择路径</span>' +
          '<button type="button" data-act="close" aria-label="关闭">✕</button></header>' +
        '<div class="crumb">' + esc(d.path) + '</div>' +
        '<ul>' +
          (d.parent ? '<li data-path="' + esc(d.parent) + '" data-dir="1" data-act="up">' +
            '<span class="icon">↰</span><span class="name">上一级</span><span class="meta"></span></li>' : '') +
          (items || '<li aria-disabled="true"><span class="name">（空目录）</span></li>') +
        '</ul>' +
        '<footer>' +
          '<span class="grow"></span>' +
          '<button type="button" data-act="pick">选择当前目录</button>' +
        '</footer>' +
        (d.storages && d.storages.length > 1
          ? '<div class="crumb">可切换根：' + d.storages.map(function (s) {
              return '<a href="#" data-root="' + esc(s.root) + '">' + esc(s.name) + '</a>';
            }).join(' · ') + '</div>'
          : '');

      dialog.querySelectorAll('[data-act=close]').forEach(function (b) {
        b.addEventListener('click', function () { dialog.close(); });
      });
      dialog.querySelectorAll('[data-root]').forEach(function (a) {
        a.addEventListener('click', function (ev) {
          ev.preventDefault();
          self._browse(dialog, a.getAttribute('data-root'));
        });
      });

      var pick = dialog.querySelector('[data-act=pick]');
      if (pick) {
        pick.disabled = !selectIsDir;
        pick.addEventListener('click', function () {
          self._picked = self._listing.path;
          dialog.close();
        });
      }

      dialog.querySelectorAll('ul li[data-path]').forEach(function (li) {
        li.addEventListener('click', function () {
          var path = li.getAttribute('data-path');
          var isDir = li.getAttribute('data-dir') === '1';
          // Clicking a directory descends; clicking a file selects. An entry
          // the interface's `select` excludes is not clickable at all, so the
          // UI cannot offer an invalid value.
          if (isDir) { self._browse(dialog, path); return; }
          if (!selectIsDir) { self._picked = path; dialog.close(); }
        });
      });
    }
  };

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  customElements.define('srcos-path-picker', PathPicker);
})();
