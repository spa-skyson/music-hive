import { useEffect } from 'react'
import { useFavorites } from '../data/index.ts'
import { usePlayer } from './context.ts'
import { playerHotkey } from './hotkeys.ts'
import { usePlayAction } from './usePlayAction.ts'

export function usePlayerHotkeys() {
  const player = usePlayer()
  const favorites = useFavorites()
  const { report } = usePlayAction()

  useEffect(() => {
    function onKeyDown(event: KeyboardEvent) {
      const hotkey = playerHotkey(event)
      if (!hotkey || !player.current || !player.sessionId || player.busy) return
      if (hotkey === 'toggle-play') {
        event.preventDefault()
        void player.togglePlay().catch(report)
      } else if (hotkey === 'skip') {
        void player.skip().catch(report)
      } else if (!favorites.loading) {
        void favorites.toggleFavorite({ track_id: player.current.id }).catch(report)
      }
    }

    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [favorites, player, report])
}
