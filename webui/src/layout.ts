import type { Flow, Layout, Node, Point } from './types'

// A node's position is either hand-placed (a saved layout, ADR-023) or derived:
// columns are the DAG layers (dependencies first), rows are order within a layer.
// Deriving is what makes a flow typed by hand — or one whose layout was never
// saved — render sanely.

export interface Placed {
  node: Node
  layer: number
  index: number
}

export function topology(flow: Flow): { order: Node[]; layers: Node[][]; depth: Map<string, number> } {
  const byId = new Map(flow.nodes.map((n) => [n.id, n]))
  const indegree = new Map<string, number>()
  const dependents = new Map<string, string[]>()
  for (const n of flow.nodes) {
    indegree.set(n.id, 0)
    dependents.set(n.id, [])
  }
  for (const n of flow.nodes) {
    for (const dep of n.depends_on ?? []) {
      if (!byId.has(dep) || dep === n.id) continue
      indegree.set(n.id, (indegree.get(n.id) ?? 0) + 1)
      dependents.get(dep)!.push(n.id)
    }
  }

  const depth = new Map<string, number>()
  const layers: Node[][] = []
  let ready = flow.nodes.filter((n) => (indegree.get(n.id) ?? 0) === 0).map((n) => n.id)
  const placed = new Set<string>()
  while (ready.length > 0) {
    const layer = ready.map((id) => byId.get(id)!).filter(Boolean)
    layers.push(layer)
    for (const n of layer) {
      depth.set(n.id, layers.length - 1)
      placed.add(n.id)
    }
    const next: string[] = []
    for (const id of ready) {
      for (const d of dependents.get(id) ?? []) {
        indegree.set(d, (indegree.get(d) ?? 0) - 1)
        if ((indegree.get(d) ?? 0) === 0) next.push(d)
      }
    }
    ready = next.sort()
  }
  // A cycle cannot happen in a validated flow; an unvalidated one shows what it
  // can rather than rendering nothing.
  for (const n of flow.nodes) if (!placed.has(n.id)) layers.push([n])

  return { order: layers.flat(), layers, depth }
}

// Geometry: one column per layer, one row per node.
export const NODE_W = 210
export const NODE_H = 96
export const COL_GAP = 90
export const ROW_GAP = 28
export const PAD_X = 32
export const PAD_Y = 28

export function derivedPosition(layers: Node[][], nodeId: string): { x: number; y: number; layer: number } {
  for (let l = 0; l < layers.length; l++) {
    const i = layers[l].findIndex((n) => n.id === nodeId)
    if (i >= 0) {
      return { x: PAD_X + l * (NODE_W + COL_GAP), y: PAD_Y + i * (NODE_H + ROW_GAP), layer: l }
    }
  }
  return { x: PAD_X, y: PAD_Y, layer: 0 }
}

// positionOf prefers a hand-placed position and falls back to the derived one,
// so a flow with a partial layout renders completely.
export function positionOf(
  layers: Node[][],
  nodeId: string,
  placed?: Layout,
): { x: number; y: number; layer: number } {
  const derived = derivedPosition(layers, nodeId)
  const p: Point | undefined = placed?.nodes?.[nodeId]
  if (p && Number.isFinite(p.x) && Number.isFinite(p.y)) {
    return { x: p.x, y: p.y, layer: derived.layer }
  }
  return derived
}

// canvasSize bounds the drawing by what is actually placed: a node dragged far
// to the right must widen the canvas, not be clipped away.
export function canvasSize(layers: Node[][], placed?: Layout): { width: number; height: number } {
  const cols = Math.max(1, layers.length)
  const rows = Math.max(1, ...layers.map((l) => l.length))
  let width = PAD_X * 2 + cols * NODE_W + (cols - 1) * COL_GAP
  let height = PAD_Y * 2 + rows * NODE_H + (rows - 1) * ROW_GAP
  for (const n of layers.flat()) {
    const p = placed?.nodes?.[n.id]
    if (!p) continue
    width = Math.max(width, p.x + NODE_W + PAD_X)
    height = Math.max(height, p.y + NODE_H + PAD_Y)
  }
  return { width, height }
}

// Ports: inputs on the left edge, outputs on the right, evenly spaced.
export function portY(index: number, count: number, nodeY: number): number {
  const usable = NODE_H - 34
  return nodeY + 30 + (usable * (index + 0.5)) / Math.max(1, count + 1)
}
