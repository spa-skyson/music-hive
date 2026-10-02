export const WEEKDAY_RU: Readonly<Record<string, string>> = {
  weekday_mon: 'понедельник', weekday_tue: 'вторник', weekday_wed: 'среда',
  weekday_thu: 'четверг', weekday_fri: 'пятница', weekday_sat: 'суббота', weekday_sun: 'воскресенье',
}

export interface MixCardData {
  kind: string
  title: string
  subtitle?: string
  tracks?: number
  ready?: boolean
  today?: boolean
  cover_track_id?: number
}

export function mixMeta(mix: MixCardData): string {
  if (mix.kind === 'later') return mix.tracks ? `${mix.tracks} в очереди` : 'пусто — добавь с плеера'
  if (mix.kind === 'favorites') return mix.tracks ? `${mix.tracks} ♥` : 'жми ♥ на треке'
  if (!mix.ready) return 'нажми «Обновить миксы»'
  return `${mix.tracks ?? 0} треков`
}

export function jobProgressLabel(job: { progress?: { message?: string; phase?: string; pct?: number }; result?: { progress?: { message?: string; phase?: string; pct?: number } } }): string {
  const progress = job.progress ?? job.result?.progress
  return progress?.message ?? (progress?.pct !== undefined ? `${progress.phase || 'job'} ${progress.pct}%` : 'Миксы генерируются в фоне')
}
