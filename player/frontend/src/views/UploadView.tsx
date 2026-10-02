import { useRef, useState, type ChangeEvent, type DragEvent } from 'react'
import { apiFetch } from '../api/client.ts'
import { useJobs, useToast, type BackgroundJob } from '../data/index.ts'
import { Button, EmptyState, ErrorState, Section } from '../ui/index.ts'
import { AUDIO_EXTENSIONS, audioExt, fileRelPath, filesFromDataTransfer, type RelativeFile } from './upload/files.ts'
import { jobProgress } from './upload/progress.ts'

const UPLOAD_BATCH = 8
const JOB_NAMES: Record<string, string> = {
  full_rescan: 'Обновление библиотеки', scan: 'Сканирование', embed: 'Аудиоэмбеддинги',
  clusters: 'Кластеры', daily: 'Daily Mix', album_tips: 'Подсказки альбомов', mix_pack: 'Набор миксов',
}
const JOB_STATES: Record<string, string> = { pending: 'В очереди', running: 'Выполняется', done: 'Готово', failed: 'Ошибка' }

interface UploadResponse { count?: number; skipped?: unknown[] }
interface JobResponse { id?: number; job_id?: number }
interface Operation { state: 'idle' | 'run' | 'done' | 'error'; title: string; detail: string; pct: number }

const initialOperation: Operation = {
  state: 'idle', title: 'Готово к загрузке', pct: 0,
  detail: 'MP3, FLAC, M4A, WAV, OGG и Opus. Скрытые файлы и прочие форматы пропускаются.',
}

export function UploadView() {
  const { showToast } = useToast()
  const { jobs, error: jobsError, retry: retryJobs } = useJobs()
  const [operation, setOperation] = useState(initialOperation)
  const [busy, setBusy] = useState(false)
  const [dragging, setDragging] = useState(false)
  const queue = useRef(Promise.resolve())
  const queueDepth = useRef(0)
  const dragDepth = useRef(0)
  const fileInput = useRef<HTMLInputElement>(null)
  const filesInput = useRef<HTMLInputElement>(null)
  const folderInput = useRef<HTMLInputElement>(null)

  function queueUpload(input: ArrayLike<File> | Iterable<File>) {
    const files = Array.from(input)
    if (!files.length) return
    const waiting = queueDepth.current
    queueDepth.current += 1
    if (waiting > 0) showToast(`Добавлено в очередь загрузки: ${files.length}`)
    queue.current = queue.current.catch(() => {}).then(() => upload(files)).finally(() => {
      queueDepth.current = Math.max(0, queueDepth.current - 1)
    })
  }

  async function upload(input: RelativeFile[]) {
    const files = input.filter((file) => AUDIO_EXTENSIONS.has(audioExt(file.name)))
    const skippedExtensions = input.length - files.length
    if (!files.length) {
      setOperation({ state: 'error', title: 'Нет аудиофайлов', pct: 0, detail: skippedExtensions
        ? `Пропущено ${skippedExtensions}: нужны MP3, FLAC, M4A, WAV, OGG или Opus.` : 'Файлы не выбраны.' })
      return
    }
    setBusy(true)
    setOperation({ state: 'run', title: 'Загрузка на диск', detail: `0 из ${files.length}`, pct: 0 })
    let saved = 0
    let skipped = skippedExtensions
    try {
      for (let index = 0; index < files.length; index += UPLOAD_BATCH) {
        const batch = files.slice(index, index + UPLOAD_BATCH)
        const body = new FormData()
        for (const file of batch) {
          body.append('file', file)
          body.append('path', fileRelPath(file))
        }
        const response = await apiFetch<UploadResponse>('/api/library/upload', { method: 'POST', body })
        saved += Number(response.count || 0)
        skipped += Array.isArray(response.skipped) ? response.skipped.length : 0
        const done = Math.min(index + batch.length, files.length)
        setOperation({ state: 'run', title: 'Загрузка на диск', pct: (done / files.length) * 55,
          detail: `${done} из ${files.length} · сохранено ${saved}${skipped ? ` · пропущено ${skipped}` : ''}` })
      }
      if (!saved) {
        setOperation({ state: 'error', title: 'Ничего не сохранено', pct: 0,
          detail: skipped ? `Пропущено ${skipped} файлов.` : 'Сервер не принял файлы.' })
        return
      }
      setOperation({ state: 'run', title: 'Постановка в очередь', pct: 95,
        detail: `На диске ${saved} файлов. Создаём фоновую задачу…` })
      const job = await apiFetch<JobResponse & { already_running?: boolean }>('/api/library/rescan', { method: 'POST', body: JSON.stringify({ full: true }) })
      const id = job.id ?? job.job_id
      setOperation({ state: 'done', title: 'Файлы загружены', pct: 100,
        detail: `${skipped ? `Добавлено ${saved}, пропущено ${skipped}.` : `Добавлено ${saved}.`} Обработка идёт в задаче #${id ?? '—'}.` })
      showToast(job.already_running ? `Обновление уже идёт (задача #${id ?? '—'})` : `Фоновая задача #${id ?? '—'} добавлена`)
    } catch (cause) {
      setOperation({ state: 'error', title: 'Операция прервалась', pct: 0,
        detail: cause instanceof Error ? cause.message : String(cause) })
    } finally {
      setBusy(false)
      for (const inputRef of [fileInput, filesInput, folderInput]) if (inputRef.current) inputRef.current.value = ''
    }
  }

  function choose(event: ChangeEvent<HTMLInputElement>) {
    if (event.target.files?.length) queueUpload(event.target.files)
  }

  function acceptsFiles(event: DragEvent): boolean {
    return [...event.dataTransfer.types].includes('Files')
  }

  function dragEnter(event: DragEvent) {
    if (!acceptsFiles(event)) return
    event.preventDefault()
    dragDepth.current += 1
    setDragging(true)
  }

  function dragLeave() {
    dragDepth.current = Math.max(0, dragDepth.current - 1)
    if (!dragDepth.current) setDragging(false)
  }

  function drop(event: DragEvent) {
    if (!acceptsFiles(event)) return
    event.preventDefault()
    dragDepth.current = 0
    setDragging(false)
    void filesFromDataTransfer(event.dataTransfer).then(queueUpload).catch((cause: unknown) => {
      setOperation({ state: 'error', title: 'Ошибка', pct: 0, detail: cause instanceof Error ? cause.message : String(cause) })
    })
  }

  return (
    <div className="mt-6 grid gap-8" onDragEnter={dragEnter} onDragLeave={dragLeave} onDragOver={(event) => {
      if (acceptsFiles(event)) { event.preventDefault(); event.dataTransfer.dropEffect = 'copy' }
    }} onDrop={drop}>
      <p className="text-muted">Перетащи файлы или папку альбома. Можно выбрать и кнопками ниже.</p>
      <input ref={fileInput} accept=".mp3,.flac,.m4a,.wav,.ogg,.opus,audio/*" aria-label="Выбрать один аудиофайл" className="sr-only" onChange={choose} type="file" />
      <input ref={filesInput} accept=".mp3,.flac,.m4a,.wav,.ogg,.opus,audio/*" aria-label="Выбрать несколько аудиофайлов" className="sr-only" multiple onChange={choose} type="file" />
      <input ref={folderInput} accept=".mp3,.flac,.m4a,.wav,.ogg,.opus,audio/*" aria-label="Выбрать папку с аудиофайлами" className="sr-only" multiple onChange={choose} type="file" {...{ webkitdirectory: '', directory: '' }} />
      <button aria-disabled={busy} className={`min-h-44 rounded-app border-2 border-dashed p-6 text-center transition ${dragging ? 'border-accent bg-accent/10' : 'border-line bg-bg2/60 hover:border-accent'} disabled:opacity-50`} disabled={busy} onClick={() => filesInput.current?.click()} type="button">
        <span className="block text-xs font-bold uppercase tracking-widest text-accent">Drag and drop</span>
        <strong className="mt-2 block text-2xl">Брось сюда музыку</strong>
        <span className="mt-2 block text-sm text-muted">Один трек, пачка файлов или целая папка. Структура альбома сохранится.</span>
      </button>
      <div className="grid gap-3 sm:grid-cols-3">
        <UploadChoice disabled={busy} kicker="Один трек" title="Файл" description="Выбрать один MP3, FLAC или другой аудиофайл" onClick={() => fileInput.current?.click()} />
        <UploadChoice disabled={busy} kicker="Несколько" title="Файлы" description="Загрузить сразу пачку треков без структуры папок" onClick={() => filesInput.current?.click()} />
        <UploadChoice disabled={busy} kicker="Каталог" title="Папка" description="Сохранить альбом целиком, с относительными путями" onClick={() => folderInput.current?.click()} />
      </div>
      <OperationBar operation={operation} />
      <Section title="Фоновые задачи" hint="Очередь хранится на сервере и продолжает работать после закрытия страницы."
        action={<span className="rounded-full border border-line px-3 py-1 text-xs text-muted">{jobs.filter((job) => ['pending', 'running'].includes(job.status)).length || 'нет'} активн.</span>}>
        <div aria-live="polite" className="grid gap-3">
          {jobsError && !jobs.length ? <ErrorState title="Не удалось загрузить задачи" description={jobsError.message} onRetry={retryJobs} /> : null}
          {!jobsError && !jobs.length ? <EmptyState title="Фоновых задач пока нет." /> : jobs.slice(0, 12).map((job) => <JobRow job={job} key={job.id} />)}
        </div>
      </Section>
    </div>
  )
}

function UploadChoice({ disabled, kicker, title, description, onClick }: { disabled: boolean; kicker: string; title: string; description: string; onClick: () => void }) {
  return <Button className="min-h-32 flex-col items-start rounded-app px-5 text-left" disabled={disabled} onClick={onClick}>
    <span className="text-xs uppercase tracking-widest text-accent">{kicker}</span><strong className="text-xl">{title}</strong><span className="font-normal text-muted">{description}</span>
  </Button>
}

function OperationBar({ operation }: { operation: Operation }) {
  const pct = Math.max(0, Math.min(100, operation.pct))
  return <div aria-live="polite" className={`rounded-app border p-4 ${operation.state === 'error' ? 'border-red-400/50 bg-red-950/20' : 'border-line bg-bg2/60'}`} role={operation.state === 'error' ? 'alert' : 'status'}>
    <div className="flex justify-between gap-4"><strong>{operation.title}</strong><span>{Math.round(pct)}%</span></div>
    <div aria-label={operation.title} aria-valuemax={100} aria-valuemin={0} aria-valuenow={Math.round(pct)} className="mt-3 h-2 overflow-hidden rounded-full bg-line" role="progressbar">
      <span className="block h-full rounded-full bg-accent transition-[width]" style={{ width: `${pct}%` }} />
    </div>
    <p className="mt-3 text-sm text-muted">{operation.detail}</p>
  </div>
}

function JobRow({ job }: { job: BackgroundJob }) {
  const progress = jobProgress(job)
  const pct = job.status === 'done' ? 100 : Math.max(0, Math.min(100, progress.pct ?? 0))
  const title = `${JOB_NAMES[job.kind] ?? job.kind ?? 'Задача'} #${job.id}`
  return <article className="rounded-xl border border-line bg-bg2/60 p-4">
    <div className="flex justify-between gap-3"><strong>{title}</strong><span className="text-sm text-muted">{JOB_STATES[job.status] ?? job.status ?? '—'}</span></div>
    <div aria-label={title} aria-valuemax={100} aria-valuemin={0} aria-valuenow={Math.round(pct)} className="mt-3 h-2 overflow-hidden rounded-full bg-line" role="progressbar"><span className="block h-full rounded-full bg-accent2" style={{ width: `${pct}%` }} /></div>
    <div className="mt-2 flex justify-between gap-3 text-sm text-muted"><span>{job.error || progress.detail || (job.status === 'pending' ? 'Ожидает запуска' : '—')}</span><span>{Math.round(pct)}%</span></div>
  </article>
}
