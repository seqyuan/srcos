import type { EditorView, Flow, FlowSummary, Layout } from './types'

// Every write goes through the server's validation: the canvas never decides on
// its own whether a wire is legal (that rule lives in internal/flow, and a second
// copy in JavaScript would drift). What this module does is *ask*.

async function request<T>(method: string, url: string, body?: unknown): Promise<T> {
  const res = await fetch(url, {
    method,
    credentials: 'same-origin',
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await res.text()
  let parsed: unknown = undefined
  try {
    parsed = text ? JSON.parse(text) : undefined
  } catch {
    throw new Error(text || `HTTP ${res.status}`)
  }
  if (!res.ok) {
    const problem = (parsed as { error?: string; problem?: string }) ?? {}
    throw new Error(problem.error || problem.problem || `HTTP ${res.status}`)
  }
  return parsed as T
}

export const listFlows = () => request<{ flows: FlowSummary[] }>('GET', '/api/admin/flows')
export const getFlow = (id: string) => request<EditorView>('GET', `/api/admin/flows/${encodeURIComponent(id)}`)
export const saveFlow = (flow: Flow) =>
  request<{ flow: Flow; valid: boolean }>('PUT', `/api/admin/flows/${encodeURIComponent(flow.id)}`, flow)
export const validateFlow = (flow: Flow) =>
  request<{ valid: boolean; problem?: string }>('POST', '/api/admin/flows/validate', flow)
// The layout has its own write: dragging a node must not run — or fail — flow
// validation, because nothing about it is part of the contract (ADR-023).
export const saveLayout = (id: string, layout: Layout) =>
  request<{ layout: Layout }>('PUT', `/api/admin/flows/${encodeURIComponent(id)}/layout`, layout)
