import type { Me } from '~/utils/types'

// The signed-in user, shared by every page.
export const useMe = () => useState<Me | null>('me', () => null)

export class ApiError extends Error {
  constructor(message: string, public status: number) {
    super(message)
  }
}

// api calls gitopsify's API: same origin, the session cookie, and the CSRF
// header on anything that changes state.
export async function api<T>(path: string, opts: { method?: string, body?: unknown, query?: Record<string, unknown> } = {}, retried = false): Promise<T> {
  const me = useMe()
  const headers: Record<string, string> = {}
  const method = opts.method || 'GET'
  if (method !== 'GET' && me.value?.csrf) headers['X-Gitopsify-CSRF'] = me.value.csrf
  try {
    return await $fetch<T>(`/api/v1${path}`, { method: method as 'GET', body: opts.body as Record<string, unknown>, query: opts.query, headers, credentials: 'same-origin' })
  } catch (e: unknown) {
    const err = e as { data?: { error?: string }, statusCode?: number, status?: number, message?: string }
    const status = err.statusCode || err.status || 0
    // Signed in again in another tab: this tab's CSRF token is the old
    // session's. Fetch the new one and try once more.
    if (status === 403 && !retried && method !== 'GET' && (err.data?.error || '').includes('X-Gitopsify-CSRF')) {
      if (await loadMe()) return api<T>(path, opts, true)
    }
    if (status === 401 && useRoute().path !== '/login') {
      me.value = null
      await navigateTo({ path: '/login', query: { redirect: useRoute().fullPath } })
    }
    throw new ApiError(err.data?.error || (status ? `request failed (${status})` : 'the server is unreachable'), status)
  }
}

// loadMe fetches the signed-in user; null when nobody is.
export async function loadMe(): Promise<Me | null> {
  const me = useMe()
  try {
    me.value = await $fetch<Me>('/api/v1/me', { credentials: 'same-origin' })
  } catch {
    me.value = null
  }
  return me.value
}

// useNotify shows a toast for an error or a success.
export function useNotify() {
  const toast = useToast()
  return {
    error(title: string, e: unknown) {
      toast.add({ title, description: e instanceof Error ? e.message : String(e), color: 'error', icon: 'i-lucide-circle-alert' })
    },
    ok(title: string, description?: string) {
      toast.add({ title, description, color: 'success', icon: 'i-lucide-circle-check' })
    }
  }
}
