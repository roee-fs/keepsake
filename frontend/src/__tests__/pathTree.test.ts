import { describe, expect, it } from 'vitest'
import { buildTree } from '../components/PathTree'

describe('buildTree', () => {
  it('returns nothing for an empty list', () => {
    expect(buildTree([])).toEqual([])
  })

  it('groups a flat list under a single top-level directory', () => {
    expect(buildTree(['a/one.md', 'a/two.md'])).toEqual([
      { name: 'a', prefix: 'a/', count: 2, children: [] },
    ])
  })

  it('nests paths that share a prefix', () => {
    const tree = buildTree(['a/b/one.md', 'a/b/two.md', 'a/c/three.md'])
    expect(tree).toEqual([
      {
        name: 'a',
        prefix: 'a/',
        count: 3,
        children: [
          { name: 'b', prefix: 'a/b/', count: 2, children: [] },
          { name: 'c', prefix: 'a/c/', count: 1, children: [] },
        ],
      },
    ])
  })

  it('builds a single-node tree for one path', () => {
    expect(buildTree(['a/one.md'])).toEqual([{ name: 'a', prefix: 'a/', count: 1, children: [] }])
  })

  it('drops a single path with no directory into no tree at all', () => {
    // The leaf segment is dropped, so a root-level file contributes zero
    // directory segments -- there is nothing to branch on.
    expect(buildTree(['readme.md'])).toEqual([])
  })

  it('sorts siblings by name', () => {
    const tree = buildTree(['z/one.md', 'a/two.md'])
    expect(tree.map((n) => n.name)).toEqual(['a', 'z'])
  })
})
