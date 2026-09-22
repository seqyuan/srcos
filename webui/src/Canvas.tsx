import { useMemo, useState } from 'react'
import type { Binding, Expose, Flow, Node, ToolView } from './types'
import { NODE_H, NODE_W, canvasSize, portY, positionOf, topology } from './layout'

// The canvas: nodes as cards, wires as curves, ports as click targets.
//
// Drawing a wire is deliberately two clicks (an output, then an input) rather
// than a drag: the check of whether the pair is even legal is the server's, and
// asking it after a drag would mean undoing a gesture. Clicking also works on a
// trackpad and in a screenshot-driven test.

export interface Selection {
  kind: 'node' | 'wire' | 'expose'
  key: string
}

interface Props {
  flow: Flow
  tools: ToolView[]
  selection: Selection | null
  onSelect: (s: Selection | null) => void
  onChange: (flow: Flow) => void
  /** The server's verdict on the current draft, shown as a banner. */
  problem?: string
}

export function Canvas({ flow, tools, selection, onSelect, onChange, problem }: Props) {
  const { layers } = useMemo(() => topology(flow), [flow])
  const size = useMemo(() => canvasSize(layers), [layers])
  const toolOf = useMemo(() => new Map(tools.map((t) => [t.id, t])), [tools])

  // The first half of a wire: an output port waiting for its input.
  const [pending, setPending] = useState<{ node: string; output: string } | null>(null)

  const bindings = flow.bindings ?? []
  const exposes = flow.expose ?? []

  function nodeAt(nodeId: string): Node | undefined {
    return flow.nodes.find((n) => n.id === nodeId)
  }
  function toolForNode(n: Node): ToolView | undefined {
    const id = n.tool.split('@')[0]
    return toolOf.get(id)
  }
  function pos(nodeId: string) {
    return positionOf(layers, nodeId)
  }

  function addBinding(toNode: string, input: string) {
    if (!pending) return
    const from = `${pending.node}.outputs.${pending.output}`
    const to = `${toNode}.inputs.${input}`
    const kept = bindings.filter((b) => b.to !== to)
    // A wire carries a path that only exists once the producer has run, so
    // drawing one *is* declaring a dependency. Adding it here keeps the graph
    // coherent; the server still refuses a flow where someone removed it by hand.
    const nodes = flow.nodes.map((n) =>
      n.id === toNode && !(n.depends_on ?? []).includes(pending.node)
        ? { ...n, depends_on: [...(n.depends_on ?? []), pending.node] }
        : n,
    )
    onChange({ ...flow, nodes, bindings: [...kept, { from, to }] })
    setPending(null)
  }

  function commit(updater: (f: Flow) => Flow) {
    onChange(updater(flow))
  }

  return (
    <div className="canvas-wrap">
      {problem && <div className="banner bad">{problem}</div>}
      {pending && (
        <div className="banner info">
          已选中输出 <code>{pending.node}.outputs.{pending.output}</code> —— 点击下游节点的输入端口完成连线
          <button className="link" onClick={() => setPending(null)}>取消</button>
        </div>
      )}
      <svg width={size.width} height={size.height} className="canvas" data-testid="canvas">
        <defs>
          <marker id="arrow" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
            <path d="M 0 0 L 10 5 L 0 10 z" fill="#8a8a8e" />
          </marker>
        </defs>

        {/* wires */}
        {bindings.map((b, i) => {
          const [fromNode, , outName] = split(b.from)
          const [toNode, , inName] = split(b.to)
          if (!nodeAt(fromNode) || !nodeAt(toNode)) return null
          const tool = nodeAt(fromNode) ? toolForNode(nodeAt(fromNode)!) : undefined
          const outIdx = (tool?.interface.outputs ?? []).findIndex((o) => o.name === outName)
          const inTool = nodeAt(toNode) ? toolForNode(nodeAt(toNode)!) : undefined
          const inIdx = (inTool?.interface.inputs ?? []).findIndex((o) => o.name === inName)
          const from = pos(fromNode)
          const to = pos(toNode)
          const outCount = (tool?.interface.outputs ?? []).length
          const inCount = (inTool?.interface.inputs ?? []).length
          const x1 = from.x + NODE_W
          const y1 = portY(Math.max(0, outIdx), outCount, from.y)
          const x2 = to.x
          const y2 = portY(Math.max(0, inIdx), inCount, to.y)
          const selected = selection?.kind === 'wire' && selection.key === String(i)
          return (
            <g key={`${b.from}->${b.to}`} className={selected ? 'wire selected' : 'wire'}
               onClick={() => onSelect({ kind: 'wire', key: String(i) })}>
              <path d={curve(x1, y1, x2, y2)} markerEnd="url(#arrow)" />
              <path d={curve(x1, y1, x2, y2)} className="wire-hit" />
            </g>
          )
        })}

        {/* nodes */}
        {flow.nodes.map((n) => {
          const tool = toolForNode(n)
          const p = pos(n.id)
          const inputs = tool?.interface.inputs ?? []
          const outputs = tool?.interface.outputs ?? []
          const selected = selection?.kind === 'node' && selection.key === n.id
          return (
            <g key={n.id} transform={`translate(${p.x},${p.y})`} className={selected ? 'node selected' : 'node'}
               onClick={() => onSelect({ kind: 'node', key: n.id })} data-testid={`node-${n.id}`}>
              <rect width={NODE_W} height={NODE_H} rx="12" />
              <text className="title" x="14" y="22">{n.id}</text>
              <text className="sub" x="14" y="40">{tool ? tool.name : `未知工具 ${n.tool}`}</text>
              <text className="meta" x="14" y="58">
                {n.tool}
                {n.when === 'always' ? ' · when: always' : ''}
                {n.retry?.max ? ` · retry≤${n.retry.max}` : ''}
              </text>
              <text className="meta" x="14" y="76">
                {(n.depends_on ?? []).length > 0 ? `← ${(n.depends_on ?? []).join(', ')}` : '第一层'}
              </text>

              {inputs.map((inp, i) => (
                <g key={inp.name} className="port" data-testid={`in-${n.id}-${inp.name}`}
                   onClick={(e) => { e.stopPropagation(); addBinding(n.id, inp.name) }}>
                  <circle cx="0" cy={portY(i, inputs.length, 0)} r={pending ? 7 : 5} className={pending ? 'in ready' : 'in'} />
                  <text x="8" y={portY(i, inputs.length, 0) + 4} className="port-label">{inp.name}</text>
                </g>
              ))}
              {outputs.map((out, i) => (
                <g key={out.name} className="port" data-testid={`out-${n.id}-${out.name}`}
                   onClick={(e) => { e.stopPropagation(); setPending({ node: n.id, output: out.name }) }}>
                  <circle cx={NODE_W} cy={portY(i, outputs.length, 0)} r="5" className="out" />
                  <text x={NODE_W - 8} y={portY(i, outputs.length, 0) + 4} className="port-label end">{out.name}</text>
                </g>
              ))}
            </g>
          )
        })}
      </svg>

      <div className="legend">
        <span><b>连线</b>：先点输出端口（右侧圆点），再点下游输入端口；合法性由服务端校验</span>
        {bindings.length > 0 && (
          <button className="link" onClick={() => {
            const idx = selection?.kind === 'wire' ? Number(selection.key) : NaN
            if (Number.isNaN(idx)) return
            const kept = bindings.filter((_, i) => i !== idx)
            onSelect(null)
            commit((f) => ({ ...f, bindings: kept }))
          }}>删除选中连线</button>
        )}
        {exposes.length > 0 && <span className="muted">{exposes.length} 个暴露参数</span>}
      </div>
    </div>
  )
}

function split(addr: string): [string, string, string] {
  const parts = addr.split('.')
  return [parts[0] ?? '', parts[1] ?? '', parts[2] ?? '']
}

function curve(x1: number, y1: number, x2: number, y2: number): string {
  const dx = Math.max(40, Math.abs(x2 - x1) / 2)
  return `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`
}

export type { Binding, Expose }
