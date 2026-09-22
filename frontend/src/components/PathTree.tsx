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

const NODE_CLASS = (selected: boolean) =>
  selected ? 'bg-hover font-medium text-fg' : 'text-fg-muted hover:bg-hover hover:text-fg'

type PathTreeProps = {
  paths: string[] | undefined
  selectedPrefix: string
  onSelect: (prefix: string) => void
}

/**
 * Built client-side from whatever paths the caller fetched, so the tree covers that
 * fetch's page rather than the whole tenant's corpus.
 */
export function PathTree({ paths, selectedPrefix, onSelect }: PathTreeProps) {
  const tree = buildTree(paths ?? [])

  return (
    <div>
      <button
        onClick={() => onSelect('')}
        className={`block w-full rounded-md px-2 py-1 text-left transition-colors ${NODE_CLASS(selectedPrefix === '')}`}
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
      className={`rounded-md px-2 py-1 text-left font-mono transition-colors ${NODE_CLASS(isSelected)}`}
    >
      {node.name} <span className="text-fg-faint tabular-nums">({node.count})</span>
    </button>
  )

  // Derived every render, not at mount: a folder is open because it is an ancestor
  // of the selection right now. Trade-off: the tree tracks the selection, so a
  // manually opened folder re-closes once the selection moves away.
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
