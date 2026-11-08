// Every page but /login needs someone signed in.
export default defineNuxtRouteMiddleware(async (to) => {
  if (to.path === '/login') return
  const me = useMe()
  if (!me.value) await loadMe()
  if (!me.value) return navigateTo({ path: '/login', query: to.fullPath !== '/' ? { redirect: to.fullPath } : {} })
  if (to.meta.role && !atLeast(me.value.serverRole, to.meta.role as 'admin')) {
    return abortNavigation(createError({ statusCode: 403, statusMessage: 'This page needs the ' + to.meta.role + ' role.' }))
  }
})
