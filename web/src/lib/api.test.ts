import { describe, expect, it } from 'vitest'

import { csrfTokenFromCookie, normalizeRequestId } from './api'

describe('normalizeRequestId', () => {
  it('keeps safe request IDs', () => {
    expect(normalizeRequestId('req_abc-123.4')).toBe('req_abc-123.4')
  })

  it('rejects unsafe or oversized request IDs', () => {
    expect(normalizeRequestId('bad value')).toBeNull()
    expect(normalizeRequestId(`${'a'.repeat(65)}`)).toBeNull()
  })
})

describe('csrfTokenFromCookie', () => {
  it('extracts only the Bablo CSRF cookie', () => {
    expect(csrfTokenFromCookie('theme=dark; bablo_csrf=csrf-token; session=opaque')).toBe('csrf-token')
    expect(csrfTokenFromCookie('bablo_session=opaque')).toBeNull()
  })
})
