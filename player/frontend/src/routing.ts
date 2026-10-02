export type View = 'home' | 'player' | 'library' | 'profile' | 'upload' | 'users'

export interface Route {
  view: View
}

const views = new Set<View>(['home', 'player', 'library', 'profile', 'upload', 'users'])

export function parseHash(hash: string): Route {
  const view = hash.replace(/^#\/?/, '').split('/')[0] as View
  return { view: views.has(view) ? view : 'home' }
}

export function routeHash(route: Route): string {
  return `#/${route.view}`
}
