import { describe, expect, it } from 'vitest'
import { provenance } from '../lib/provenance'

describe('provenance', () => {
  it('reads generated and a list of verifications', () => {
    expect(
      provenance({
        generated: { by: 'agent/1', at: '2026-06-26T00:00:00Z' },
        verified: [
          { by: 'process:nightly', at: '2026-06-24T00:00:00Z' },
          { by: 'human:ann', at: '2026-06-25T00:00:00Z' },
        ],
      }),
    ).toEqual({
      generated: { by: 'agent/1', at: '2026-06-26T00:00:00Z' },
      verified: [
        { by: 'process:nightly', at: '2026-06-24T00:00:00Z' },
        { by: 'human:ann', at: '2026-06-25T00:00:00Z' },
      ],
    })
  })

  // OKF §5.2: a bare mapping is a one-element list.
  it('reads a bare verified mapping as one event', () => {
    expect(provenance({ verified: { by: 'human:ann', at: '2026-06-25T00:00:00Z' } }).verified).toEqual([
      { by: 'human:ann', at: '2026-06-25T00:00:00Z' },
    ])
  })

  it('skips malformed entries and absent fields', () => {
    expect(provenance({ generated: 'yes', verified: ['x', { at: '2026-06-25T00:00:00Z' }, { by: 7 }] })).toEqual({
      generated: null,
      verified: [],
    })
    expect(provenance({})).toEqual({ generated: null, verified: [] })
  })
})
