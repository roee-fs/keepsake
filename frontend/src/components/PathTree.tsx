import { useEffect, useRef, useState } from 'react'

type TreeNode = {
  name: string
  prefix: string
  count: number
  children: TreeNode[]
}

// Only directory segments form the tree; the leaf (file) segment is dropped,
// since leaves are what the table lists, not what the tree navigates.
function buildTree(dirSegments: string[][], prefix: string): TreeNode[] {
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
        children: buildTree(rest, childPrefix),
      }
    })
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
  const tree = buildTree((paths ?? []).map((p) => p.split('/').slice(0, -1)), '')

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

  // Computed once at mount: expanding to show the initially-selected prefix
  // shouldn't fight a later manual toggle when an unrelated render happens.
  const [initialOpen] = useState(() => selectedPrefix.startsWith(node.prefix))
  const detailsRef = useRef<HTMLDetailsElement>(null)
  useEffect(() => {
    if (detailsRef.current) detailsRef.current.open = initialOpen
  }, [initialOpen])

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
