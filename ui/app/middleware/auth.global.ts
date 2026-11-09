// Every page but /login and /cli-login needs someone signed in.
export default defineNuxtRouteMiddleware(async (to) => {
  // The command-line sign-in's page needs no session: the browser gets none.
  if (to.path === '/login' || to.path === '/cli-login') return
  const me = useMe()
  if (!me.value) await loadMe()
  if (!me.value) return navigateTo({ path: '/login', query: to.fullPath !== '/' ? { redirect: to.fullPath } : {} })
  if (to.meta.role && !atLeast(me.value.serverRole, to.meta.role as 'admin')) {
    return abortNavigation(createError({ statusCode: 403, statusMessage: 'This page needs the ' + to.meta.role + ' role.' }))
  }
})
