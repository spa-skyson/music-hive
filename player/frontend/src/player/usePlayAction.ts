import { apiFetch } from '../api/client.ts'
import { useToast } from '../data/index.ts'

export function usePlayAction() {
  const { showToast } = useToast()
  const report = (cause: unknown) => showToast(cause instanceof Error ? cause.message : 'Не удалось выполнить действие', { variant: 'error' })
  async function act(action: () => Promise<unknown>) {
    try { await action() } catch (cause) { report(cause) }
  }
  const play = (action: () => Promise<unknown>) => act(async () => { await action(); window.location.assign('#/player') })
  const addToLater = (trackId: number, toast: string) => act(async () => {
      await apiFetch('/api/later', { method: 'POST', body: JSON.stringify({ track_id: trackId }) })
      showToast(toast)
  })
  return { act, addToLater, play, report }
}
