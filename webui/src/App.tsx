import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Canvas, type Selection } from './Canvas'
import { Inspector } from './Inspector'
import { getFlow, saveFlow, saveLayout, validateFlow } from './api'
import type { EditorView, Expose, Flow, Layout, ToolView } from './types'

// The console's flow editor: one screen, one job — make the wiring visible and
// editable, and let the *server* be the judge of whether it is legal.

interface Boot {
  flowId: string
  user: string
}

declare global {
  interface Window { __SRCOS__?: Boot }
}

export function App() {
  const boot = window.__SRCOS__ as Boot | undefined
  const flowId = boot?.flowId ?? ''

  const [draft, setDraft] = useState<Flow | null>(null)
  const [tools, setTools] = useState<ToolView[]>([])
  const [selection, setSelection] = useState<Selection | null>(null)
  const [problem, setProblem] = useState<string | undefined>()
  const [status, setStatus] = useState<string>('')
  const [dirty, setDirty] = useState(false)
  const [error, setError] = useState<string | undefined>()
  // The layout is its own document (ADR-023): dragging a node changes it, not
  // the flow, so it is saved separately and never makes the flow look dirty.
  const [layout, setLayout] = useState<Layout>({ nodes: {} })
  const [suggested, setSuggested] = useState<Expose[]>([])

  const layoutTimer = useRef<number | null>(null)
  useEffect(() => () => { if (layoutTimer.current) window.clearTimeout(layoutTimer.current) }, [])

  useEffect(() => {
    if (!flowId) {
      setError('地址里没有流程 id（应为 /admin/flows/<id>/edit）')
      return
    }
    getFlow(flowId)
      .then((view: EditorView) => {
        setDraft(view.flow)
        setTools(view.tools ?? [])
        setLayout(view.layout ?? { nodes: {} })
        setSuggested(view.suggestedExpose ?? [])
        setProblem(view.valid ? undefined : view.problem)
      })
      .catch((e: Error) => setError(e.message))
  }, [flowId])

  // Ask the server about every edit: the type rules live in one place, and a
  // canvas that disagreed with them would be worse than no canvas.
  useEffect(() => {
    if (!draft || !dirty) return
    const timer = setTimeout(() => {
      validateFlow(draft)
        .then((res) => {
          setProblem(res.valid ? undefined : res.problem)
          setStatus(res.valid ? '校验通过（未保存）' : '')
        })
        .catch((e: Error) => setProblem(e.message))
    }, 150)
    return () => clearTimeout(timer)
  }, [draft, dirty])

  const change = useCallback((next: Flow) => {
    setDraft(next)
    setDirty(true)
    setStatus('有未保存的改动')
  }, [])

  // A drag settles into a save a moment later: dragging continuously would
  // otherwise write the file on every pointer move.
  const moveNode = useCallback((next: Layout) => {
    setLayout(next)
    if (!flowId) return
    if (layoutTimer.current) window.clearTimeout(layoutTimer.current)
    layoutTimer.current = window.setTimeout(() => {
      saveLayout(flowId, next)
        .then(() => setStatus('布局已保存'))
        .catch((e: Error) => setProblem(`布局保存失败：${e.message}`))
    }, 400)
  }, [flowId])

  // Fill in the inputs nothing feeds yet, with the source the server suggested
  // (sample.<input> by convention). It stays a draft edit: the flow is written
  // only by 保存, through the validated path.
  const fillExpose = useCallback(() => {
    if (!draft || suggested.length === 0) return
    const existing = new Set((draft.expose ?? []).map((e) => `${e.node}.${e.input}`))
    const add = suggested.filter((e) => !existing.has(`${e.node}.${e.input}`))
    change({ ...draft, expose: [...(draft.expose ?? []), ...add] })
    setSuggested((s) => s.filter((e) => !add.includes(e)))
    setStatus(`已补齐 ${add.length} 个 expose —— 检查取值来源后点保存`)
  }, [draft, suggested, change])

  async function save() {
    if (!draft) return
    setStatus('保存中…')
    try {
      const res = await saveFlow(draft)
      setDraft(res.flow)
      setDirty(false)
      setProblem(undefined)
      setStatus('已保存到 flow.yaml')
    } catch (e) {
      setProblem((e as Error).message)
      setStatus('保存失败')
    }
  }

  function addNode(toolId: string) {
    if (!draft) return
    const base = toolId.replace(/[^a-z0-9_-]/gi, '-').toLowerCase()
    let id = base
    let n = 2
    while (draft.nodes.some((x) => x.id === id)) id = `${base}-${n++}`
    const tool = tools.find((t) => t.id === toolId)
    change({ ...draft, nodes: [...draft.nodes, { id, tool: `${toolId}@${tool?.version ?? '0.0.0'}` }] })
    setSelection({ kind: 'node', key: id })
  }

  function removeNode(id: string) {
    if (!draft) return
    change({
      ...draft,
      nodes: draft.nodes
        .filter((n) => n.id !== id)
        .map((n) => ({ ...n, depends_on: (n.depends_on ?? []).filter((d) => d !== id) })),
      bindings: (draft.bindings ?? []).filter((b) => !b.from.startsWith(id + '.') && !b.to.startsWith(id + '.')),
      expose: (draft.expose ?? []).filter((e) => e.node !== id),
    })
    setSelection(null)
  }

  const toolPicker = useMemo(() => tools.filter((t) => t.kind === 'task'), [tools])

  if (error) {
    return <div className="shell"><div className="banner bad">{error}</div></div>
  }
  if (!draft) {
    return <div className="shell"><p className="muted">载入流程…</p></div>
  }

  return (
    <div className="shell">
      <header className="top">
        <div>
          <h1>{draft.name || draft.id}</h1>
          <p className="muted">
            <code>{draft.id}</code> · v{draft.version} · {draft.nodes.length} 节点 ·{' '}
            {sampleColumns(draft).length > 0 ? `样本表列：${sampleColumns(draft).join(', ')}` : '无样本维度'}
          </p>
        </div>
        <div className="actions">
          <select defaultValue="" onChange={(e) => { if (e.target.value) addNode(e.target.value); e.target.value = '' }}>
            <option value="">+ 添加节点…</option>
            {toolPicker.map((t) => <option key={t.id} value={t.id}>{t.id}（{t.name}）</option>)}
          </select>
          {selection?.kind === 'node' && (
            <button className="danger" onClick={() => removeNode(selection.key)}>删除节点</button>
          )}
          <button className="primary" onClick={save} disabled={!dirty} data-testid="save">保存</button>
          <a className="link" href="/admin">返回控制台</a>
        </div>
      </header>

      <div className={'status' + (problem ? ' bad' : dirty ? ' warn' : '')}>
        {problem ? problem : status || '未修改'}
      </div>

      <div className="body">
        <Canvas
          flow={draft}
          tools={tools}
          layout={layout}
          selection={selection}
          onSelect={setSelection}
          onChange={change}
          onLayoutChange={moveNode}
          suggested={suggested}
          onApplySuggestions={fillExpose}
          problem={undefined}
        />
        <Inspector flow={draft} tools={tools} selection={selection} onChange={change} />
      </div>
    </div>
  )
}

function sampleColumns(flow: Flow): string[] {
  const cols = new Set<string>()
  for (const e of flow.expose ?? []) {
    if (e.from.startsWith('sample.')) cols.add(e.from.slice('sample.'.length))
  }
  return [...cols]
}
