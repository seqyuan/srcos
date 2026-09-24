import { useMemo, useState } from 'react'
import type { Binding, Expose, Flow, Layout, Node, Point, ToolView } from './types'
import { NODE_H, NODE_W, canvasSize, portY, positionOf, topology } from './layout'

// The canvas: nodes as cards, wires as curves, ports as click targets.
//
// Drawing a wire is deliberately two clicks (an output, then an input) rather
// than a drag: the check of whether the pair is even legal is the server's, and
// asking it after a drag would mean undoing a gesture. Clicking also works on a
// trackpad and in a screenshot-driven test.
//
// Moving a node *is* a drag, because there is nothing to validate: a position is
// not part of the flow (ADR-023). It is saved on its own, so a drag can never
// make a valid flow look broken.

export interface Selection {
  kind: 'node' | 'wire' | 'expose'
  key: string
}

interface Props {
  flow: Flow
  tools: ToolView[]
  layout: Layout
  selection: Selection | null
  onSelect: (s: Selection | null) => void
  onChange: (flow: Flow) => void
  /** Called with the full coordinate map after a drag settles. */
  onLayoutChange: (layout: Layout) => void
  /** Required inputs nothing feeds yet (the server derived them). */
  suggested: Expose[]
  onApplySuggestions: () => void
  /** The server's verdict on the current draft, shown as a banner. */
  problem?: string
}

// dragState is a node being moved: the pointer offset inside the card, and the
// live position the card is rendered at.
interface DragState {
  id: string
  offX: number
  offY: number
  x: number
  y: number
}

export function Canvas({
  flow, tools, layout, selection, onSelect, onChange, onLayoutChange,
  suggested, onApplySuggestions, problem,
}: Props) {
  const { layers } = useMemo(() => topology(flow), [flow])
  const [drag, setDrag] = useState<DragState | null>(null)
  const size = useMemo(() => canvasSize(layers, layout), [layers, layout])
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
    if (drag?.id === nodeId) {
      return { x: drag.x, y: drag.y, layer: positionOf(layers, nodeId, layout).layer }
    }
    return positionOf(layers, nodeId, layout)
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

  // ── dragging ──────────────────────────────────────────────────────────
  // The pointer is captured by the node, so move/up come back to it even when
  // the cursor leaves the card (or the window). Positions are converted to
  // canvas coordinates through the SVG's own box, which also survives scrolling.
  function canvasPoint(e: React.PointerEvent<SVGGElement>): Point {
    const svg = e.currentTarget.ownerSVGElement
    if (!svg) return { x: e.clientX, y: e.clientY }
    const rect = svg.getBoundingClientRect()
    return { x: e.clientX - rect.left, y: e.clientY - rect.top }
  }

  function startDrag(e: React.PointerEvent<SVGGElement>, id: string) {
    if (e.button !== 0) return
    const p = pos(id)
    const at = canvasPoint(e)
    e.currentTarget.setPointerCapture?.(e.pointerId)
    setDrag({ id, offX: at.x - p.x, offY: at.y - p.y, x: p.x, y: p.y })
    onSelect({ kind: 'node', key: id })
  }

  function moveDrag(e: React.PointerEvent<SVGGElement>) {
    if (!drag) return
    const at = canvasPoint(e)
    setDrag({ ...drag, x: at.x - drag.offX, y: at.y - drag.offY })
  }

  function endDrag(e: React.PointerEvent<SVGGElement>) {
    if (!drag) return
    e.currentTarget.releasePointerCapture?.(e.pointerId)
    // Round to whole units: sub-pixel positions would make layout.yaml churn on
    // every drag and read like noise in a diff.
    const placed = { x: Math.round(drag.x), y: Math.round(drag.y) }
    const id = drag.id
    setDrag(null)
    onLayoutChange({ ...layout, nodes: { ...layout.nodes, [id]: placed } })
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
          const moving = drag?.id === n.id
          return (
            <g key={n.id} transform={`translate(${p.x},${p.y})`}
               className={(selected ? 'node selected' : 'node') + (moving ? ' moving' : '')}
               onClick={() => onSelect({ kind: 'node', key: n.id })}
               onPointerDown={(e) => startDrag(e, n.id)}
               onPointerMove={moveDrag}
               onPointerUp={endDrag}
               onPointerCancel={endDrag}
               data-testid={`node-${n.id}`}>
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
                   onPointerDown={(e) => e.stopPropagation()}
                   onClick={(e) => { e.stopPropagation(); addBinding(n.id, inp.name) }}>
                  <circle cx="0" cy={portY(i, inputs.length, 0)} r={pending ? 7 : 5} className={pending ? 'in ready' : 'in'} />
                  <text x="8" y={portY(i, inputs.length, 0) + 4} className="port-label">{inp.name}</text>
                </g>
              ))}
              {outputs.map((out, i) => (
                <g key={out.name} className="port" data-testid={`out-${n.id}-${out.name}`}
                   onPointerDown={(e) => e.stopPropagation()}
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
        <span className="muted">拖动节点可摆放位置（自动保存到 layout.yaml）</span>
        {suggested.length > 0 && (
          <button className="link" data-testid="fill-expose" onClick={onApplySuggestions}>
            补齐 expose（{suggested.length} 个未接的必填输入）
          </button>
        )}
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
