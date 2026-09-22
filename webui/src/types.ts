// The wire contract, mirroring internal/flow (yaml tags and json tags are the
// same names there, so this file is a transcription, not a translation).

export type InputType =
  | 'string' | 'int' | 'float' | 'bool' | 'enum'
  | 'file' | 'directory' | 'dirpath' | 'path'

export interface Input {
  name: string
  type: InputType
  label?: string
  description?: string
  required?: boolean
  default?: unknown
  values?: string[]
  min?: number
  max?: number
  from?: string
  select?: 'file' | 'directory'
  files?: string[]
}

export interface Output {
  name: string
  type: 'file' | 'directory'
  label?: string
  description?: string
  provides?: string[]
}

export interface Interface {
  inputs?: Input[]
  outputs?: Output[]
}

export interface Resources {
  cpu: number
  memory: string
  walltime?: string
  queue?: string
  gpu?: number
}

export interface Node {
  id: string
  tool: string
  depends_on?: string[]
  when?: 'on_success' | 'always'
  retry?: { max: number }
}

export interface Binding {
  from: string // <node>.outputs.<name>
  to: string // <node>.inputs.<name>
}

export interface Expose {
  node: string
  input: string
  from: string // user | sample.<col> | output.<name>
}

export interface Flow {
  schemaVersion: number
  id: string
  version: string
  name: string
  description?: string
  nodes: Node[]
  bindings?: Binding[]
  expose?: Expose[]
}

export interface ToolView {
  id: string
  version: string
  name: string
  kind: 'task' | 'service'
  backend: string
  interface: Interface
  resources: Resources
}

export interface EditorView {
  flow: Flow
  tools: ToolView[]
  valid: boolean
  problem?: string
}

export interface FlowSummary {
  id: string
  version: string
  name: string
  description?: string
  nodes: number
  layers: number
  sampleColumns?: string[]
  valid: boolean
  problem?: string
}

export const WHEN_ON_SUCCESS = 'on_success'
export const WHEN_ALWAYS = 'always'
