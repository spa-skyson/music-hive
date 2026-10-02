import { useEffect, useState } from 'react'
import { parseHash, routeHash, type Route } from './routing.ts'

export function useHashRoute() {
  const [route, setRoute] = useState<Route>(() => parseHash(location.hash))

  useEffect(() => {
    const onHashChange = () => setRoute(parseHash(location.hash))
    globalThis.addEventListener('hashchange', onHashChange)
    return () => globalThis.removeEventListener('hashchange', onHashChange)
  }, [])

  function navigate(next: Route) {
    const hash = routeHash(next)
    if (location.hash === hash) setRoute(next)
    else location.hash = hash
  }

  return { route, navigate }
}
