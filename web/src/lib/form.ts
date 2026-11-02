/** The first validation message of a TanStack Form field, for a Mantine input's `error` prop. */
export function fieldError(meta: { errors: unknown[]; isTouched: boolean }): string | undefined {
  if (!meta.isTouched) return undefined
  const e = meta.errors.find((x) => x !== undefined && x !== null && x !== '')
  return e === undefined ? undefined : String(e)
}

export const required = (label: string) => ({ value }: { value: unknown }) =>
  (typeof value === 'string' ? value.trim() === '' : value === undefined || value === null) ? `${label} is required` : undefined

export const SLUG_RE = /^[a-z0-9][a-z0-9-]{1,46}[a-z0-9]$/
