// A port of `Resolve` in okf/links.go. The two MUST agree, or the body links and
// the "Links to" list name different concepts.
const EXTERNAL = /^[a-z][a-z0-9+.-]*:\/\/|^(?:mailto|tel):|^\/\//i

export function isExternal(target: string): boolean {
  return EXTERNAL.test(target)
}

/** The concept path a link in `source`'s body names, or null if it names no concept. */
export function resolveLink(target: string, source: string): string | null {
  if (isExternal(target)) return null
  target = target.split('#', 1)[0].split('?', 1)[0]
  if (!target) return null
  target = target.replace(/\.md$/, '')
  let directory = source.includes('/') ? source.slice(0, source.lastIndexOf('/')) : ''
  if (target.startsWith('/')) {
    target = target.replace(/^\/+/, '')
    directory = ''
  }
  const out: string[] = []
  let escaped = false
  for (const segment of `${directory}/${target}`.split('/')) {
    if (segment === '' || segment === '.') continue
    if (segment !== '..') out.push(segment)
    else if (out.length > 0) out.pop()
    else escaped = true
  }
  return escaped || out.length === 0 ? null : out.join('/')
}
