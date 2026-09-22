import { Link } from '@tanstack/react-router'
import { forceCenter, forceLink, forceManyBody, forceSimulation, type SimulationNodeDatum } from 'd3-force'
import { useMemo, useState } from 'react'
import type { Graph } from '../client'

// The validated dark categorical palette, in fixed slot order.
const SERIES = ['#3987e5', '#d95926', '#199e70', '#c98500', '#d55181', '#008300', '#9085e9', '#e66767']
const OTHER = '#8a8f98'
const RADIUS = 6

/** Type → colour. Types past the eighth fold into one "Other" grey rather than a generated hue. */
export function typeColors(types: string[]): { colors: Map<string, string>; other: boolean } {
  const sorted = [...new Set(types)].sort()
  const colors = new Map(sorted.map((t, i) => [t, SERIES[i] ?? OTHER]))
  return { colors, other: sorted.length > SERIES.length }
}

type Node = SimulationNodeDatum & Graph['nodes'][number]

function layout(graph: Graph) {
  const nodes: Node[] = graph.nodes.map((n) => ({ ...n }))
  const links = graph.edges.map(([source, target]) => ({ source, target }))
  // Run to rest up front: a static layout is enough to read, and nothing animates.
  forceSimulation(nodes)
    .force('link', forceLink<Node, { source: string; target: string }>(links).id((n) => n.path).distance(60))
    .force('charge', forceManyBody().strength(-180))
    .force('center', forceCenter(0, 0))
    .stop()
    .tick(300)
  const byPath = new Map(nodes.map((n) => [n.path, n]))
  const xs = nodes.map((n) => n.x ?? 0)
  const ys = nodes.map((n) => n.y ?? 0)
  const pad = 40
  const box = {
    x: Math.min(...xs) - pad,
    y: Math.min(...ys) - pad,
    w: Math.max(...xs) - Math.min(...xs) + pad * 2,
    h: Math.max(...ys) - Math.min(...ys) + pad * 2,
  }
  return { nodes, byPath, box }
}

export function ConceptGraph({ graph, tenant }: { graph: Graph; tenant: string }) {
  const { nodes, byPath, box } = useMemo(() => layout(graph), [graph])
  const { colors, other } = useMemo(
    () => typeColors(graph.nodes.filter((n) => !n.missing).map((n) => n.type)),
    [graph],
  )
  const [hovered, setHovered] = useState<string | null>(null)

  const neighbours = useMemo(() => {
    if (!hovered) return null
    const set = new Set([hovered])
    for (const [s, t] of graph.edges) {
      if (s === hovered) set.add(t)
      if (t === hovered) set.add(s)
    }
    return set
  }, [hovered, graph])

  const active = hovered ? byPath.get(hovered) : undefined
  const hasMissing = graph.nodes.some((n) => n.missing)
  // Labels on every node only while the graph is small enough for them not to collide.
  const labelAll = nodes.length <= 40

  return (
    <div className="flex flex-col gap-3">
      <ul className="flex flex-wrap gap-x-4 gap-y-1 text-sm text-fg-muted">
        {[...colors].filter(([, c]) => c !== OTHER).map(([type, color]) => (
          <li key={type} className="flex items-center gap-1.5">
            <span className="size-2.5 rounded-full" style={{ background: color }} />
            {type}
          </li>
        ))}
        {other && (
          <li className="flex items-center gap-1.5">
            <span className="size-2.5 rounded-full" style={{ background: OTHER }} />
            Other
          </li>
        )}
        {hasMissing && (
          <li className="flex items-center gap-1.5">
            <span className="size-2.5 rounded-full border border-dashed border-fg-muted" />
            Linked but never written
          </li>
        )}
      </ul>

      <div className="relative rounded-md border border-line bg-surface">
        <svg
          viewBox={`${box.x} ${box.y} ${box.w} ${box.h}`}
          className="h-[640px] w-full"
          role="img"
          aria-label={`Link graph of ${nodes.length} concepts`}
        >
          <g stroke="var(--color-fg-faint)" strokeWidth={1}>
            {graph.edges.map(([s, t]) => {
              const a = byPath.get(s)
              const b = byPath.get(t)
              if (!a || !b) return null
              const lit = neighbours?.has(s) && neighbours.has(t)
              return (
                <line
                  key={`${s}\u0000${t}`}
                  x1={a.x}
                  y1={a.y}
                  x2={b.x}
                  y2={b.y}
                  strokeOpacity={neighbours ? (lit ? 0.9 : 0.1) : 0.5}
                />
              )
            })}
          </g>
          {nodes.map((n) => {
            const dim = neighbours && !neighbours.has(n.path)
            const circle = (
              <circle
                r={RADIUS}
                fill={n.missing ? 'var(--color-surface)' : colors.get(n.type)}
                stroke={n.missing ? 'var(--color-fg-muted)' : 'var(--color-surface)'}
                strokeWidth={2}
                strokeDasharray={n.missing ? '2 2' : undefined}
              />
            )
            return (
              <g
                key={n.path}
                transform={`translate(${n.x},${n.y})`}
                opacity={dim ? 0.25 : 1}
                onMouseEnter={() => setHovered(n.path)}
                onMouseLeave={() => setHovered(null)}
              >
                {/* A hit target larger than the mark. */}
                <circle r={RADIUS * 2} fill="transparent" />
                {n.missing ? (
                  circle
                ) : (
                  <Link to="/concepts/$" params={{ _splat: n.path }} search={{ tenant }}>
                    {circle}
                  </Link>
                )}
                {(labelAll || n.path === hovered) && (
                  <text x={RADIUS + 4} y={4} fontSize={11} fill="var(--color-fg-muted)" className="pointer-events-none select-none">
                    {n.path.split('/').at(-1)}
                  </text>
                )}
              </g>
            )
          })}
        </svg>

        {active && (
          <div className="pointer-events-none absolute top-3 left-3 max-w-80 rounded-md border border-line bg-base px-3 py-2 text-sm">
            <div className="font-medium text-fg">{active.missing ? 'Not written yet' : active.title}</div>
            <div className="font-mono text-fg-muted">{active.path}</div>
            {!active.missing && <div className="text-fg-faint">{active.type}</div>}
          </div>
        )}
      </div>
    </div>
  )
}
