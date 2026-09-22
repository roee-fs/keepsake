import { createMemoryHistory, createRouter, RouterContextProvider } from '@tanstack/react-router'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import { BodyLink } from '../components/BodyLink'
import { routeTree } from '../routeTree.gen'

const router = createRouter({ routeTree, history: createMemoryHistory() })

function render(href: string) {
  return renderToStaticMarkup(
    <RouterContextProvider router={router}>
      <BodyLink href={href} title="Tip" source="a/b" tenant="t1">
        text
      </BodyLink>
    </RouterContextProvider>,
  )
}

describe('BodyLink', () => {
  it('keeps a markdown link title on an internal link', () => {
    const html = render('../infra/postgres.md')
    expect(html).toContain('href="/concepts/infra/postgres?tenant=t1"')
    expect(html).toContain('title="Tip"')
  })

  it('keeps a markdown link title on an external link', () => {
    expect(render('https://example.com')).toContain('title="Tip"')
  })
})
