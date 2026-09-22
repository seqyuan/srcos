import type { Expose, Flow, Node, ToolView } from './types'

// The inspector is the other half of the canvas: the graph shows *how* the steps
// are wired, this panel edits *what each step is told* — which inputs come from
// the sample table, which the user fills, which are fed by an upstream output,
// and which the platform decides (a node's own output path).

interface Props {
  flow: Flow
  tools: ToolView[]
  selection: { kind: 'node' | 'wire' | 'expose'; key: string } | null
  onChange: (flow: Flow) => void
}

export function Inspector({ flow, tools, selection, onChange }: Props) {
  if (!selection || selection.kind !== 'node') {
    return (
      <aside className="inspector">
        <h2>检查器</h2>
        <p className="muted">选中一个节点，编辑它的依赖、重试、以及每个输入从哪里取值。</p>
      </aside>
    )
  }
  const node = flow.nodes.find((n) => n.id === selection.key)
  if (!node) return <aside className="inspector"><p className="muted">节点已删除</p></aside>
  return <NodeInspector flow={flow} tools={tools} node={node} onChange={onChange} />
}

// NodeInspector is a separate component so `node` is narrowed once, and the
// callbacks below cannot see it as optional.
function NodeInspector({ flow, tools, node, onChange }: {
  flow: Flow
  tools: ToolView[]
  node: Node
  onChange: (flow: Flow) => void
}) {
  const tool = tools.find((t) => t.id === node.tool.split('@')[0])

  function updateNode(patch: Partial<Node>) {
    onChange({ ...flow, nodes: flow.nodes.map((n) => (n.id === node.id ? { ...n, ...patch } : n)) })
  }
  function setExpose(input: string, from: string) {
    const kept = (flow.expose ?? []).filter((e) => !(e.node === node.id && e.input === input))
    const next: Expose[] = from === '' ? kept : [...kept, { node: node.id, input, from }]
    // A binding and an expose for the same input would be two sources, which the
    // server refuses; wiring one clears the other here so the UI cannot build a
    // flow the server would reject.
    const bindings = from.startsWith('upstream:')
      ? [...(flow.bindings ?? []), { from: from.slice('upstream:'.length), to: `${node.id}.inputs.${input}` }]
      : (flow.bindings ?? []).filter((b) => b.to !== `${node.id}.inputs.${input}`)
    onChange({ ...flow, expose: next, bindings })
  }
  function exposeValue(input: string): string {
    const e = (flow.expose ?? []).find((x) => x.node === node.id && x.input === input)
    if (e) return e.from
    const b = (flow.bindings ?? []).find((x) => x.to === `${node.id}.inputs.${input}`)
    return b ? `upstream:${b.from}` : ''
  }

  const upstreamOptions = (flow.bindings ?? [])
    .filter((b) => b.to.startsWith(node.id + '.inputs.'))
    .length

  return (
    <aside className="inspector">
      <h2>{node.id}</h2>
      <p className="muted">{tool ? `${tool.name} · ${tool.kind} · ${tool.backend}` : '未知工具'}</p>

      <label>工具
        <select value={node.tool} onChange={(e) => updateNode({ tool: e.target.value })}>
          {tools.map((t) => (
            <option key={t.id} value={`${t.id}@${t.version}`} disabled={t.kind !== 'task'}>
              {t.id}@{t.version}{t.kind !== 'task' ? '（服务不能当节点）' : ''}
            </option>
          ))}
        </select>
      </label>

      <label>依赖（逗号分隔的节点 id）
        <input value={(node.depends_on ?? []).join(', ')}
               onChange={(e) => updateNode({ depends_on: e.target.value.split(',').map((s) => s.trim()).filter(Boolean) })} />
      </label>

      <div className="row">
        <label>上游失败时
          <select value={node.when ?? 'on_success'} onChange={(e) => updateNode({ when: e.target.value as Node['when'] })}>
            <option value="on_success">on_success（默认）</option>
            <option value="always">always（出报告）</option>
          </select>
        </label>
        <label>重试上限
          <input type="number" min={0} max={5} value={node.retry?.max ?? 0}
                 onChange={(e) => updateNode({ retry: { max: Number(e.target.value) } })} />
        </label>
      </div>

      <h3>输入</h3>
      <table className="io">
        <tbody>
          {(tool?.interface.inputs ?? []).map((inp) => (
            <tr key={inp.name}>
              <td>
                <code>{inp.name}</code>
                {inp.required ? <span className="req">*</span> : null}
                <div className="muted">{inp.type}{inp.select ? ` · ${inp.select}` : ''}</div>
              </td>
              <td>
                <select value={exposeValue(inp.name)} onChange={(e) => setExpose(inp.name, e.target.value)}>
                  <option value="">工具默认值 / 未接线</option>
                  <option value="user">用户运行时填（--param）</option>
                  {sampleColumns(flow).map((col) => (
                    <option key={col} value={`sample.${col}`}>样本表列 {col}</option>
                  ))}
                  {upstreamEdges(flow, node.id).map((e) => (
                    <option key={e} value={`upstream:${e}`}>{e}</option>
                  ))}
                  {(tool?.interface.outputs ?? []).map((out) => (
                    <option key={out.name} value={`output.${out.name}`}>本节点产物 {out.name}</option>
                  ))}
                </select>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {upstreamOptions > 0 && <p className="muted">已连线的输入也可以在画布上直接删除连线。</p>}

      <h3>产物</h3>
      <ul className="outputs">
        {(tool?.interface.outputs ?? []).map((out) => (
          <li key={out.name}>
            <code>{out.name}</code> <span className="muted">{out.type}</span>
            <div className="path">/flow/runs/&lt;run&gt;/nodes/{node.id}/&lt;样本&gt;/{out.name}</div>
          </li>
        ))}
        {(tool?.interface.outputs ?? []).length === 0 && <li className="muted">该工具没有声明产物</li>}
      </ul>
    </aside>
  )
}

function sampleColumns(flow: Flow): string[] {
  const cols = new Set<string>()
  for (const e of flow.expose ?? []) {
    if (e.from.startsWith('sample.')) cols.add(e.from.slice('sample.'.length))
  }
  // A column that does not exist yet can still be typed in below.
  return [...cols]
}

function upstreamEdges(flow: Flow, nodeId: string): string[] {
  return (flow.bindings ?? [])
    .filter((b) => b.to.startsWith(nodeId + '.inputs.'))
    .map((b) => b.from)
}
