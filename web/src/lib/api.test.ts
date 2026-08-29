import { describe, expect, it } from 'vitest'

import { normalizeRequestId } from './api'

describe('normalizeRequestId', () => {
  it('keeps safe request IDs', () => {
    expect(normalizeRequestId('req_abc-123.4')).toBe('req_abc-123.4')
  })

  it('rejects unsafe or oversized request IDs', () => {
    expect(normalizeRequestId('bad value')).toBeNull()
    expect(normalizeRequestId(`${'a'.repeat(65)}`)).toBeNull()
  })
})
