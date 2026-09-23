import { describe, expect, it } from 'vitest'
import { typeColors } from '../components/ConceptGraph'

describe('typeColors', () => {
  it('keeps a type on the same colour whatever order the nodes arrive in', () => {
    expect(typeColors(['b', 'a', 'a']).colors).toEqual(typeColors(['a', 'b']).colors)
  })

  it('folds types past the eighth into one grey instead of generating a hue', () => {
    const { colors, other } = typeColors('abcdefghij'.split(''))
    expect(other).toBe(true)
    expect(new Set([...colors.values()]).size).toBe(9)
    expect(colors.get('i')).toBe(colors.get('j'))
  })
})
