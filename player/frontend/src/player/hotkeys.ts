export type PlayerHotkey = 'toggle-play' | 'skip' | 'like'

interface HotkeyEvent {
  code: string
  key: string
  target: EventTarget | null
}

export function playerHotkey(event: HotkeyEvent): PlayerHotkey | null {
  const target = event.target as { closest?: (selector: string) => unknown } | null
  if (target?.closest?.('input, textarea, select, [contenteditable], button, a, summary, [role="button"]')) return null
  if (event.code === 'Space') return 'toggle-play'
  if (event.key.toLowerCase() === 'n') return 'skip'
  if (event.key.toLowerCase() === 'l') return 'like'
  return null
}
