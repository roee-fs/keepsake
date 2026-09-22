import { useEffect, useRef } from 'react'

export type TreeNode = {
  name: string
  prefix: string
  count: number
  children: TreeNode[]
}

function groupBySegment(dirSegments: string[][], prefix: string): TreeNode[] {
  const groups = new Map<string, string[][]>()
  for (const segments of dirSegments) {
    if (segments.length === 0) continue
    const [head, ...rest] = segments
    const group = groups.get(head) ?? []
    group.push(rest)
    groups.set(head, group)
  }
  return [...groups.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, rest]) => {
      const childPrefix = `${prefix}${name}/`
      return {
        name,
        prefix: childPrefix,
        count: rest.length,
        children: groupBySegment(rest, childPrefix),
      }
    })
}

// Only directory segments form the tree; the leaf (file) segment is dropped,
// since leaves are what the table lists, not what the tree navigates.
export function buildTree(paths: string[]): TreeNode[] {
  return groupBySegment(
    paths.map((p) => p.split('/').slice(0, -1)),
    '',
  )
}

type PathTreeProps = {
  paths: string[] | undefined
  selectedPrefix: string
  onSelect: (prefix: string) => void
}

/**
 * Built client-side from whatever paths the caller fetched -- there is no
 * "list distinct prefixes" endpoint, so the tree's depth is bounded by that
 * fetch's page size, not the whole tenant's corpus.
 */
export function PathTree({ paths, selectedPrefix, onSelect }: PathTreeProps) {
  const tree = buildTree(paths ?? [])

  return (
    <div className="text-sm">
      <button
        onClick={() => onSelect('')}
        className={`block w-full rounded px-2 py-1 text-left ${selectedPrefix === '' ? 'bg-gray-200 font-medium' : 'hover:bg-gray-100'}`}
      >
        All
      </button>
      {tree.map((node) => (
        <TreeItem key={node.prefix} node={node} selectedPrefix={selectedPrefix} onSelect={onSelect} />
      ))}
    </div>
  )
}

function TreeItem({
  node,
  selectedPrefix,
  onSelect,
}: {
  node: TreeNode
  selectedPrefix: string
  onSelect: (prefix: string) => void
}) {
  const isSelected = selectedPrefix === node.prefix
  const label = (
    <button
      onClick={(event) => {
        event.stopPropagation()
        onSelect(node.prefix)
      }}
      className={`rounded px-2 py-1 text-left ${isSelected ? 'bg-gray-200 font-medium' : 'hover:bg-gray-100'}`}
    >
      {node.name} <span className="text-gray-400">({node.count})</span>
    </button>
  )

  // Derived on every render, not just at mount: the tree's job is to show
  // where the current selection sits in the hierarchy, so an ancestor of the
  // selected prefix stays open because it IS an ancestor right now, not
  // because it was one when this node first mounted. Trade-off: this also
  // re-closes a folder the user opened manually once the selection moves
  // away from it -- the tree tracks the selection, not manual expand state.
  const isAncestorOfSelection = selectedPrefix.startsWith(node.prefix)
  const detailsRef = useRef<HTMLDetailsElement>(null)
  useEffect(() => {
    if (detailsRef.current) detailsRef.current.open = isAncestorOfSelection
  }, [isAncestorOfSelection])

  if (node.children.length === 0) {
    return <div className="ml-3">{label}</div>
  }

  return (
    <details className="ml-3" ref={detailsRef}>
      <summary className="cursor-pointer list-none">{label}</summary>
      <div className="ml-3">
        {node.children.map((child) => (
          <TreeItem key={child.prefix} node={child} selectedPrefix={selectedPrefix} onSelect={onSelect} />
        ))}
      </div>
    </details>
  )
}
