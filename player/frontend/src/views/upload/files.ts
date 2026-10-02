export const AUDIO_EXTENSIONS = new Set(['.mp3', '.flac', '.m4a', '.wav', '.ogg', '.opus'])

export interface RelativeFile extends File {
  readonly _relPath?: string
}

interface FileSystemEntry {
  readonly isFile: boolean
  readonly isDirectory: boolean
  readonly name: string
}

interface FileSystemFileEntry extends FileSystemEntry {
  file: (success: (file: File) => void, error: (cause: DOMException) => void) => void
}

interface FileSystemDirectoryReader {
  readEntries: (success: (entries: FileSystemEntry[]) => void, error: (cause: DOMException) => void) => void
}

interface FileSystemDirectoryEntry extends FileSystemEntry {
  createReader: () => FileSystemDirectoryReader
}

type EntryDataTransferItem = DataTransferItem & { webkitGetAsEntry: () => FileSystemEntry | null }

export function audioExt(name: string): string {
  const index = String(name || '').lastIndexOf('.')
  return index < 0 ? '' : String(name).slice(index).toLowerCase()
}

export function fileRelPath(file: RelativeFile): string {
  return String(file._relPath || file.webkitRelativePath || file.name || '').replaceAll('\\', '/')
}

export function withRelPath(file: File, relativePath: string): RelativeFile {
  try {
    Object.defineProperty(file, '_relPath', { value: relativePath.replaceAll('\\', '/'), configurable: true })
  } catch {
    // Some browser File implementations are not extensible; the filename remains usable.
  }
  return file
}

function readAllEntries(reader: FileSystemDirectoryReader): Promise<FileSystemEntry[]> {
  return new Promise((resolve, reject) => {
    const entries: FileSystemEntry[] = []
    const readNext = () => {
      reader.readEntries((batch) => {
        if (!batch.length) {
          resolve(entries)
          return
        }
        entries.push(...batch)
        readNext()
      }, reject)
    }
    readNext()
  })
}

async function filesFromEntry(entry: FileSystemEntry | null, prefix = ''): Promise<RelativeFile[]> {
  if (!entry) return []
  if (entry.isFile) {
    const file = await new Promise<File>((resolve, reject) => (entry as FileSystemFileEntry).file(resolve, reject))
    return [withRelPath(file, prefix + file.name)]
  }
  if (!entry.isDirectory) return []
  const nextPrefix = prefix + entry.name + '/'
  const children = await readAllEntries((entry as FileSystemDirectoryEntry).createReader())
  const nested = await Promise.all(children.map((child) => filesFromEntry(child, nextPrefix)))
  return nested.flat()
}

export async function filesFromDataTransfer(dataTransfer: DataTransfer): Promise<RelativeFile[]> {
  const items = Array.from(dataTransfer.items) as EntryDataTransferItem[]
  if (items.some((item) => typeof item.webkitGetAsEntry === 'function' && item.webkitGetAsEntry())) {
    const groups = await Promise.all(
      items
        .filter((item) => item.kind === 'file')
        .map((item) => filesFromEntry(item.webkitGetAsEntry?.() ?? null)),
    )
    return groups.flat()
  }
  return Array.from(dataTransfer.files)
}
