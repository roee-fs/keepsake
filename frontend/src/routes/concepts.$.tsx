import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link, useSearch } from '@tanstack/react-router'
import Markdown from 'react-markdown'
import { conceptDetailConceptsPathGet } from '../client'
import { Frontmatter } from '../components/Frontmatter'
import { RevisionList } from '../components/RevisionList'

export const Route = createFileRoute('/concepts/$')({
  component: Detail,
})

function LinkList({ title, paths, tenant }: { title: string; paths: string[]; tenant: string }) {
  return (
    <div>
      <h2 className="mb-2 text-sm font-medium text-gray-500">{title}</h2>
      {paths.length === 0 ? (
        <p className="text-sm text-gray-500">None.</p>
      ) : (
        <ul className="text-sm">
          {paths.map((path) => (
            <li key={path}>
              <Link to="/concepts/$" params={{ _splat: path }} search={{ tenant }} className="font-mono text-blue-600 hover:underline">
                {path}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
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
    return <div className="p-4 text-sm text-gray-500">No concept path given.</div>
  }

  if (!tenant) {
    return (
      <div className="p-4 text-sm text-gray-500">
        This concept has no tenant selected.{' '}
        <Link to="/concepts" className="text-blue-600 hover:underline">
          Pick one from Browse.
        </Link>
      </div>
    )
  }

  if (detail.isLoading) {
    return <div className="m-4 h-48 animate-pulse rounded border bg-gray-100" />
  }

  const concept = detail.data
  if (!concept) {
    return <div className="p-4 text-sm text-gray-500">Not found.</div>
  }

  return (
    <div className="flex flex-col gap-6 p-4">
      <div>
        <div className="text-sm text-gray-500">
          {concept.type} · v{concept.version} · <span className="font-mono">{path}</span>
        </div>
        <h1 className="text-xl font-semibold">{concept.title}</h1>
        <p className="text-gray-600">{concept.description}</p>
      </div>

      <div className="prose prose-sm max-w-none">
        <Markdown>{concept.body}</Markdown>
      </div>

      <div>
        <h2 className="mb-2 text-sm font-medium text-gray-500">Frontmatter</h2>
        <Frontmatter frontmatter={concept.frontmatter} />
      </div>

      <div className="grid grid-cols-2 gap-6">
        <LinkList title="Links to" paths={concept.links} tenant={tenant} />
        <LinkList title="Linked from" paths={concept.backlinks} tenant={tenant} />
      </div>

      <div>
        <h2 className="mb-2 text-sm font-medium text-gray-500">Revision history</h2>
        <RevisionList revisions={concept.revisions} />
      </div>
    </div>
  )
}
