// Writes internal/epubcfi/testdata/golden.json: CFIs made and resolved by foliate's
// own epubcfi.js (copied byte-identical from the player's vendored foliate-js, pin in
// PIN.txt) on jsdom, over the fixture EPUBs, for internal/epubcfi's golden tests
// (Bridge Phase 3c, decision 27). Run it only when the pin changes:
//
//   cd scripts/cfi-golden && npm ci && node gen.mjs
//
// It also rebuilds testdata/synthetic.epub from synthetic/. CI runs Go only.
import fs from 'node:fs'
import path from 'node:path'
import crypto from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { JSDOM } from 'jsdom'
import * as CFI from './epubcfi.js'

const here = path.dirname(fileURLToPath(import.meta.url))
const testdata = path.resolve(here, '../../internal/epubcfi/testdata')
globalThis.NodeFilter = new JSDOM('').window.NodeFilter

let skipped = 0
const sha256 = (b) => crypto.createHash('sha256').update(b).digest('hex')

// synthetic.epub: mimetype stored first, the rest deflated, no extra attributes.
function buildSynthetic() {
  const out = path.join(testdata, 'synthetic.epub')
  fs.rmSync(out, { force: true })
  const src = path.join(here, 'synthetic')
  execFileSync('zip', ['-X0q', out, 'mimetype'], { cwd: src })
  execFileSync('zip', ['-Xr9Dq', out, 'META-INF', 'OEBPS'], { cwd: src })
}

function readZip(file) {
  const names = execFileSync('unzip', ['-Z1', file]).toString('utf8').split('\n').filter(Boolean)
  const out = new Map()
  for (const n of names) {
    if (n.endsWith('/')) continue
    out.set(n, execFileSync('unzip', ['-p', file, n.replace(/([[\]*?])/g, '\\$1')], { maxBuffer: 64 << 20 }))
  }
  return out
}

const parse = (str, type) => new JSDOM(str, { contentType: type }).window.document

const BLOCK = new Set(['p', 'div', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'li', 'pre', 'blockquote', 'br', 'tr', 'td', 'th', 'table', 'section', 'article', 'header', 'footer', 'hr', 'dt', 'dd', 'figcaption', 'ul', 'ol', 'dl'])

// The spike's walkText (tools/cfi/epublib.mjs), UTF-16 offsets.
function walkText(doc) {
  const body = doc.body ?? doc.getElementsByTagName('body')[0] ?? doc.documentElement
  const nodes = [], starts = [], breaks = []
  let off = 0
  const visit = (n) => {
    if (n.nodeType === 3 || n.nodeType === 4) {
      nodes.push(n); starts.push(off); off += n.nodeValue.length; return
    }
    if (n.nodeType !== 1) return
    const name = n.localName?.toLowerCase()
    if (name === 'script' || name === 'style') return
    const blk = BLOCK.has(name)
    if (blk) breaks.push(off)
    for (const c of n.childNodes) visit(c)
    if (blk) breaks.push(off)
  }
  visit(body)
  return { nodes, starts, text: nodes.map((n) => n.nodeValue).join(''), breaks: [...new Set(breaks)].sort((a, b) => a - b) }
}

function locate(w, off, isEnd) {
  const target = isEnd ? Math.max(off - 1, 0) : off
  let lo = 0, hi = w.starts.length - 1, ans = 0
  while (lo <= hi) { const m = (lo + hi) >> 1; if (w.starts[m] <= target) { ans = m; lo = m + 1 } else hi = m - 1 }
  while (ans < w.nodes.length - 1 && w.nodes[ans].nodeValue.length === 0) ans++
  return { node: w.nodes[ans], offset: off - w.starts[ans] }
}

// The ranges each section is tested over: words (every step-th), runs of eight
// words, and every block's trimmed text (sentences over inline elements).
function rangesOf(text, breaks, step) {
  const words = [...text.matchAll(/\S+/g)].map((m) => [m.index, m.index + m[0].length])
  const out = []
  words.forEach(([s, e], k) => { if (k % step === 0) out.push([s, e]) })
  for (let k = 0; k + 7 < words.length; k += step * 3) out.push([words[k][0], words[k + 7][1]])
  const cuts = [0, ...breaks, text.length]
  for (let k = 0; k + 1 < cuts.length; k += step > 1 ? 5 : 1) {
    const seg = text.slice(cuts[k], cuts[k + 1])
    const l = seg.length - seg.trimStart().length, r = seg.trimEnd().length
    if (r > l) out.push([cuts[k] + l, cuts[k] + r])
  }
  return out
}

function book(name, step) {
  const file = path.join(testdata, `${name}.epub`)
  const zip = readZip(file)
  const container = parse(zip.get('META-INF/container.xml').toString('utf8'), 'application/xml')
  const opfPath = container.getElementsByTagName('rootfile')[0].getAttribute('full-path')
  const opf = parse(zip.get(opfPath).toString('utf8'), 'application/xml')
  const base = path.posix.dirname(opfPath)
  const manifest = new Map()
  for (const it of opf.getElementsByTagName('item'))
    manifest.set(it.getAttribute('id'), { href: path.posix.join(base === '.' ? '' : base, decodeURIComponent(it.getAttribute('href'))), mediaType: it.getAttribute('media-type') })
  const itemrefs = [...opf.getElementsByTagName('itemref')]
  const cfis = CFI.fromElements(itemrefs)
  const spine = itemrefs.map((ir, index) => {
    const m = manifest.get(ir.getAttribute('idref'))
    return { index, idref: ir.getAttribute('idref'), href: m.href, media_type: m.mediaType, linear: ir.getAttribute('linear') !== 'no', cfi: cfis[index] }
  })
  const sections = []
  const all = []
  for (const s of spine) {
    if (!/html/.test(s.media_type)) continue
    const raw = zip.get(s.href).toString('utf8')
    const type = s.media_type === 'text/html' ? 'text/html' : 'application/xhtml+xml'
    const w = walkText(parse(raw, type))
    const ranges = []
    for (const [start, end] of rangesOf(w.text, w.breaks, step)) {
      const a = locate(w, start, false), b = locate(w, end, true)
      const r = a.node.ownerDocument.createRange()
      // jsdom (26.1) refuses a Range boundary in a CDATASection ("Offset out of
      // bound"), which a browser takes: such a range is left out (counted). Ranges
      // beside it still test the chunk offsets CDATA adds to.
      if (a.node.nodeType === 4 || b.node.nodeType === 4) { skipped++; continue }
      r.setStart(a.node, a.offset); r.setEnd(b.node, b.offset)
      const cfi = CFI.joinIndir(s.cfi, CFI.fromRange(r))
      // Resolved on a fresh parse, as the reader does.
      const parts = CFI.parse(cfi)
      ;(parts.parent ?? parts).shift()
      const text = CFI.toRange(parse(raw, type), parts).toString()
      // jsdom's Range.toString leaves CDATA out (a browser's keeps it, CDATASection
      // being a Text): such a range's text is not foliate's to hold the Go port to.
      const overCDATA = w.nodes.some((n, k) => n.nodeType === 4 && w.starts[k] < end && w.starts[k] + n.nodeValue.length > start)
      ranges.push(overCDATA ? { start, end, cfi, text, cdata: true } : { start, end, cfi, text })
      all.push(cfi)
    }
    sections.push({ index: s.index, href: s.href, text_len16: w.text.length, text_sha256: sha256(Buffer.from(w.text, 'utf8')), breaks16: w.breaks, ranges })
  }
  // compare(): neighbours and far pairs; collapse(): both ends of some ranges.
  const compares = []
  for (let k = 0; k + 1 < all.length; k += Math.max(1, Math.floor(all.length / 150))) {
    const j = (k * 7919) % all.length
    compares.push({ a: all[k], b: all[k + 1], cmp: CFI.compare(all[k], all[k + 1]) })
    compares.push({ a: all[k], b: all[j], cmp: CFI.compare(all[k], all[j]) })
  }
  const collapses = all.filter((_, k) => k % 13 === 0).map((c) => ({ cfi: c, start: CFI.collapse(c), end: CFI.collapse(c, true) }))
  return { name, sha256: sha256(fs.readFileSync(file)), opf: opfPath, spine, sections, compares, collapses }
}

buildSynthetic()
const golden = {
  generator: 'scripts/cfi-golden/gen.mjs',
  epubcfi_sha256: sha256(fs.readFileSync(path.join(here, 'epubcfi.js'))),
  books: [book('synthetic', 1), book('alice', 40)],
}
fs.writeFileSync(path.join(testdata, 'golden.json'), JSON.stringify(golden) + '\n')
console.log(`${skipped} CDATA ranges left out; ` + golden.books.map((b) => `${b.name}: ${b.sections.reduce((n, s) => n + s.ranges.length, 0)} ranges`).join(', '))
