import { Card, Group, Table, Text, TextInput, UnstyledButton } from '@mantine/core'
import { IconArrowDown, IconArrowUp, IconArrowsSort, IconSearch } from '@tabler/icons-react'
import {
  flexRender, getCoreRowModel, getFilteredRowModel, getSortedRowModel, useReactTable,
  type ColumnDef, type Row, type SortingState,
} from '@tanstack/react-table'
import type { ReactNode } from 'react'
import { useView } from '@/lib/viewPrefs'

export type { ColumnDef } from '@tanstack/react-table'

/**
 * A sortable, filterable table (SM-UI-001: TanStack Table on Mantine). Column headers sort on click or Enter and
 * announce their order (aria-sort); the filter matches every column's text. The sort order and filter text are view
 * preferences kept per `view` (lib/viewPrefs, TanStack Store).
 */
export function DataTable<T>({ view, data, columns, empty, filterLabel, onRowClick, rowLabel, getRowId, defaultSort = [], toolbar, caption }: {
  view: string
  data: T[]
  // eslint-free project: column value types vary per column
  columns: ColumnDef<T, any>[]
  empty: ReactNode
  /** Shows a filter box with this label. */
  filterLabel?: string
  onRowClick?: (row: T) => void
  rowLabel?: (row: T) => string
  getRowId?: (row: T) => string
  defaultSort?: SortingState
  toolbar?: ReactNode
  caption?: string
}) {
  const [prefs, setPrefs] = useView(view, { sort: defaultSort, filter: '' })
  const table = useReactTable({
    data, columns, getRowId,
    state: { sorting: prefs.sort, globalFilter: prefs.filter },
    onSortingChange: (u) => setPrefs({ sort: typeof u === 'function' ? u(prefs.sort) : u }),
    onGlobalFilterChange: (u) => setPrefs({ filter: String(typeof u === 'function' ? u(prefs.filter) : u ?? '') }),
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    globalFilterFn: 'includesString',
  })
  const rows = table.getRowModel().rows
  const open = (r: Row<T>) => onRowClick?.(r.original)
  return (
    <>
      {(filterLabel || toolbar) && (
        <Group mb="xs" justify="space-between" gap="xs">
          {filterLabel ? (
            <TextInput size="xs" w={260} aria-label={filterLabel} placeholder={filterLabel} leftSection={<IconSearch size={14} />}
              value={prefs.filter} onChange={(e) => table.setGlobalFilter(e.currentTarget.value)} />
          ) : <span />}
          {toolbar}
        </Group>
      )}
      <Card p={0} style={{ overflowX: 'auto' }}>
        <Table highlightOnHover={!!onRowClick} aria-label={caption}>
          <Table.Thead>
            {table.getHeaderGroups().map((hg) => (
              <Table.Tr key={hg.id}>
                {hg.headers.map((h) => {
                  const sorted = h.column.getIsSorted()
                  const label = h.isPlaceholder ? null : flexRender(h.column.columnDef.header, h.getContext())
                  return (
                    <Table.Th key={h.id} w={h.column.columnDef.size && h.column.columnDef.size !== 150 ? h.column.columnDef.size : undefined}
                      aria-sort={sorted === 'asc' ? 'ascending' : sorted === 'desc' ? 'descending' : h.column.getCanSort() ? 'none' : undefined}>
                      {h.column.getCanSort() ? (
                        <UnstyledButton onClick={h.column.getToggleSortingHandler()} style={{ font: 'inherit', fontWeight: 600 }}>
                          <Group gap={4} wrap="nowrap">
                            {label}
                            {sorted === 'asc' ? <IconArrowUp size={12} aria-hidden /> : sorted === 'desc' ? <IconArrowDown size={12} aria-hidden />
                              : <IconArrowsSort size={12} aria-hidden opacity={0.4} />}
                          </Group>
                        </UnstyledButton>
                      ) : label}
                    </Table.Th>
                  )
                })}
              </Table.Tr>
            ))}
          </Table.Thead>
          <Table.Tbody>
            {rows.map((r) => (
              <Table.Tr key={r.id} style={onRowClick ? { cursor: 'pointer' } : undefined} tabIndex={onRowClick ? 0 : undefined}
                aria-label={rowLabel?.(r.original)} onClick={onRowClick ? () => open(r) : undefined}
                onKeyDown={onRowClick ? (e) => { if (e.key === 'Enter') open(r) } : undefined}>
                {r.getVisibleCells().map((c) => <Table.Td key={c.id}>{flexRender(c.column.columnDef.cell, c.getContext())}</Table.Td>)}
              </Table.Tr>
            ))}
            {rows.length === 0 && (
              <Table.Tr>
                <Table.Td colSpan={columns.length}><Text size="sm" c="dimmed" ta="center" p="md">{data.length ? 'Nothing matches the filter.' : empty}</Text></Table.Td>
              </Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Card>
    </>
  )
}
