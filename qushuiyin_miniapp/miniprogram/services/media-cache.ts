const pending = new Set<string>()

function unlink(path: string): Promise<boolean> {
  return new Promise<boolean>(resolve => wx.getFileSystemManager().unlink({ filePath: path,
    success() { resolve(true) },
    fail(error) { resolve(/no such file|enoent/i.test(error.errMsg)) },
  })).catch(() => false)
}

export async function releaseTemporaryMedia(path: string) {
  pending.add(path)
  if (await unlink(path)) pending.delete(path)
}

export async function clearMediaCache() {
  const paths = Array.from(pending)
  for (const path of paths) if (await unlink(path)) pending.delete(path)
  return { hadFiles: paths.length > 0, failed: paths.some(path => pending.has(path)) }
}
