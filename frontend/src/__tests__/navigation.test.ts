import { createMemoryHistory, createRouter } from '@tanstack/react-router'
import { describe, expect, it } from 'vitest'
import { routeTree } from '../routeTree.gen'

describe('navigation', () => {
  it.each(['/', '/concepts', '/graph'])('keeps the selected tenant on a link to %s', async (to) => {
    const router = createRouter({ routeTree, history: createMemoryHistory({ initialEntries: ['/concepts?tenant=t1'] }) })
    await router.load().catch(() => {})
    expect(router.buildLocation({ to }).search).toEqual({ tenant: 't1' })
  })
})
