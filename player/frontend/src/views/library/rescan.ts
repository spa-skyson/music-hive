import { apiFetch } from '../../api/client.ts'

interface RescanResponse { id?: number; job_id?: number; already_running?: boolean }

/** Запросить сканирование библиотеки: full=false — быстрый scan, true — full_rescan.
 * Возвращает человекочитаемый итог для тоста и aria-live-статуса (#56). */
export async function requestLibraryRescan(full: boolean): Promise<string> {
  const job = await apiFetch<RescanResponse>('/api/library/rescan', { method: 'POST', body: JSON.stringify({ full }) })
  const id = job.id ?? job.job_id ?? '—'
  return job.already_running
    ? `Обновление уже идёт (задача #${id})`
    : `Поставлено: обновление библиотеки (задача #${id})`
}
