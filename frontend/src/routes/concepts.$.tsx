import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link, useSearch } from '@tanstack/react-router'
import type { ReactNode } from 'react'
import Markdown from 'react-markdown'
import { conceptDetailConceptsPathGet } from '../client'
import { Frontmatter } from '../components/Frontmatter'
import { RevisionList } from '../components/RevisionList'
import { isExternal, resolveLink } from '../lib/links'
import { HttpError } from '../lib/session'

export const Route = createFileRoute('/concepts/$')({
  component: Detail,
})

const SECTION_HEADING = 'mb-3 text-md font-semibold tracking-tight'

function LinkList({ title, paths, tenant }: { title: string; paths: string[]; tenant: string }) {
  return (
    <div>
      <h2 className={SECTION_HEADING}>{title}</h2>
      {paths.length === 0 ? (
        <p className="text-fg-muted">None.</p>
      ) : (
        <ul className="flex flex-col gap-1">
          {paths.map((path) => (
            <li key={path}>
              <Link to="/concepts/$" params={{ _splat: path }} search={{ tenant }} className="font-mono text-link hover:underline">
                {path}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function BodyLink({ href, children, source, tenant }: { href?: string; children?: ReactNode; source: string; tenant: string }) {
  if (href?.startsWith('#') || (href && isExternal(href))) {
    return <a href={href} className="text-link hover:underline">{children}</a>
  }
  const target = href ? resolveLink(href, source) : null
  if (!target) return <span>{children}</span>
  return (
    <Link to="/concepts/$" params={{ _splat: target }} search={{ tenant }} className="text-link hover:underline">
      {children}
    </Link>
  )
}

function Detail() {
  const { _splat: path } = Route.useParams()
  const { tenant } = useSearch({ strict: false })

  const detail = useQuery({
    queryKey: ['concept-detail', tenant, path],
    queryFn: async () =>
      (
        await conceptDetailConceptsPathGet({
          // Safe: `enabled` keeps this from running before both are set.
          path: { path: path! },
          query: { tenant: tenant! },
          throwOnError: true,
        })
      ).data,
    enabled: !!tenant && !!path,
  })

  if (!path) {
    return <div className="text-fg-muted">No concept path given.</div>
  }

  if (!tenant) {
    return (
      <div className="text-fg-muted">
        This concept has no tenant selected.{' '}
        <Link to="/concepts" className="text-link hover:underline">
          Pick one from Browse.
        </Link>
      </div>
    )
  }

  if (detail.isLoading) {
    return <div className="h-48 animate-pulse rounded-md border border-line bg-surface" />
  }

  // `throwOnError` is global, so a genuine 404 arrives as an error like any other.
  // Only that one is a missing concept; everything else is a failed read, and
  // reporting it as "Not found." would describe the store rather than the request.
  if (detail.isError) {
    const missing = detail.error instanceof HttpError && detail.error.status === 404
    return (
      <div className="text-fg-muted">
        {missing ? 'Not found.' : 'Could not load this concept.'}
      </div>
    )
  }

  const concept = detail.data
  if (!concept) {
    return <div className="text-fg-muted">Not found.</div>
  }

  return (
    <div className="flex flex-col gap-8">
      <div>
        <div className="font-mono text-sm text-fg-muted">
          {concept.type} · v{concept.version} · <span className="text-fg-faint">{path}</span>
        </div>
        <h1 className="mt-1 text-lg font-semibold tracking-tight">{concept.title}</h1>
        <p className="mt-1 text-fg-muted">{concept.description}</p>
      </div>

      {/* Raw HTML stays off: this body is agent-written, so the markdown AST is
          the trust boundary, not a styling choice. */}
      <div className="markdown max-w-[68ch]">
        <Markdown
          components={{ a: ({ href, children }) => <BodyLink href={href} source={path} tenant={tenant}>{children}</BodyLink> }}
        >
          {concept.body}
        </Markdown>
      </div>

      <div>
        <h2 className={SECTION_HEADING}>Frontmatter</h2>
        <Frontmatter frontmatter={concept.frontmatter} />
      </div>

      <div className="grid grid-cols-2 gap-8">
        <LinkList title="Links to" paths={concept.links} tenant={tenant} />
        <LinkList title="Linked from" paths={concept.backlinks} tenant={tenant} />
      </div>

      <div>
        <h2 className={SECTION_HEADING}>Revision history</h2>
        <RevisionList revisions={concept.revisions} />
      </div>
    </div>
  )
}
