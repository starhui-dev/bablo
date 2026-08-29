export class ApiError extends Error {
  readonly status: number
  readonly requestId: string | null
  readonly code: string | null

  constructor(message: string, status: number, requestId: string | null, code: string | null) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.requestId = requestId
    this.code = code
  }
}

export function normalizeRequestId(value: string | null | undefined): string | null {
  const trimmed = value?.trim() ?? ''
  if (!trimmed || trimmed.length > 64 || !/^[A-Za-z0-9._-]+$/.test(trimmed)) {
    return null
  }
  return trimmed
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    credentials: 'include',
    ...init,
    headers: {
      Accept: 'application/json',
      ...init?.headers,
    },
  })
  const requestId = normalizeRequestId(response.headers.get('X-Request-ID'))
  const body = (await response.json().catch(() => null)) as {
    error?: { message?: string; code?: string; request_id?: string }
  } | null

  if (!response.ok) {
    throw new ApiError(
      body?.error?.message ?? `请求失败（HTTP ${response.status}）`,
      response.status,
      normalizeRequestId(body?.error?.request_id) ?? requestId,
      body?.error?.code ?? null,
    )
  }
  return body as T
}

export const api = {
  get<T>(path: string): Promise<T> {
    return request<T>(path)
  },
  post<T>(path: string, payload: unknown): Promise<T> {
    return request<T>(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    })
  },
}
