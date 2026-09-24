/**
 * The build: one file dsh can serve.
 *
 * dsh's client module system reads `exports["./client"]` **as bytes** and serves
 * them at `/plugins/<id>/client.js`; the file itself must register with
 * `window.__ModuleLoader__.load({id, factory})` and request its externals through
 * the factory's `require` (measured from an installed plugin's built
 * `lib/client.js`).
 *
 * That envelope is small enough to write here, which is the point: this plugin
 * needs **no dsh build toolchain and no dependencies**, so it can be built,
 * reviewed and tested with the Node that ships on the machine. `src/*.js` stays
 * ordinary ESM (so `node --test` imports it directly), and this transform — strip
 * relative imports, turn a bare import into `require`, drop `export` — is a
 * concatenation, not a bundler.
 *
 * Run: `npm run build` (also runs on `npm pack`, so what gets installed is
 * always the current source).
 */

import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const pkg = JSON.parse(readFileSync(join(here, 'package.json'), 'utf8'))

/** Dependency order: every top-level `const` must be declared before its use. */
const MODULES = ['src/address.js', 'src/config.js', 'src/api.js', 'src/provider.js', 'src/client.js']

/**
 * Turn one ESM module into a fragment of the factory body.
 * @param source - the module's text.
 * @returns the fragment.
 */
function transform(source) {
  const out = []
  for (const line of source.split('\n')) {
    const trimmed = line.trimStart()
    // A relative import is inlined; a bare one becomes a require of the shared
    // module table (react is part of dsh's implicit client baseline).
    const imported = /^import\s+(.+?)\s+from\s+['"]([^'"]+)['"]\s*;?$/.exec(trimmed)
    if (imported !== null) {
      const [, bindings, specifier] = imported
      if (specifier.startsWith('.')) continue
      const names = bindings.replace(/[{}]/g, '').split(',').map((s) => s.trim()).filter(Boolean)
        .map((s) => {
          const [name, alias] = s.split(/\s+as\s+/)
          return alias === undefined ? name : `${name}: ${alias}`
        })
      out.push(`const { ${names.join(', ')} } = require(${JSON.stringify(specifier)});`)
      continue
    }
    // A type-only import is erased (it is erased in TypeScript too).
    if (trimmed.startsWith('import type ')) continue
    if (trimmed.startsWith('import ')) {
      throw new Error(`build: unsupported import form: ${trimmed}`)
    }
    out.push(line
      .replace(/^export\s+(async\s+)?function\s/, (_, async) => `${async ?? ''}function `)
      .replace(/^export\s+const\s/, 'const ')
      .replace(/^export\s+class\s/, 'class '))
  }
  return out.join('\n')
}

const body = MODULES.map((file) => {
  const source = readFileSync(join(here, file), 'utf8')
  return `//#region ${file}\n${transform(source)}\n//#endregion`
}).join('\n\n')

const bundle = `window.__ModuleLoader__.load({
\tid: ${JSON.stringify(pkg.name)},
\tfactory: (require) => {
\t\tvar module = { exports: {} };
\t\tvar exports = module.exports;
\t\tObject.defineProperty(exports, Symbol.toStringTag, { value: "Module" });
${body.split('\n').map((line) => (line === '' ? '' : `\t\t${line}`)).join('\n')}
\t\texports.apply = apply;
\t\texports.inject = inject;
\t\texports.SrcosPane = SrcosPane;
\t\texports.srcosDefinition = srcosDefinition;
\t\treturn module.exports;
\t}
});
`

mkdirSync(join(here, 'lib'), { recursive: true })
writeFileSync(join(here, 'lib/client.js'), bundle)
process.stdout.write(`built lib/client.js (${bundle.length} bytes)\n`)
