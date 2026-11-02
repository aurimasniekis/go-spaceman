import { ActionIcon, Button, Group, Select, Stack, Text } from '@mantine/core'
import { IconPlus, IconTrash } from '@tabler/icons-react'
import type { GrantInput, Role } from '@/api/client'
import { useSpaces } from '@/api/queries'

const ROLES: { value: Role; label: string }[] = [
  { value: 'viewer', label: 'Viewer' },
  { value: 'operator', label: 'Operator' },
  { value: 'admin', label: 'Admin' },
]

/** Edits role grants: a role in one space, or in every space ("*"). */
export function GrantsEditor({ value, onChange }: { value: GrantInput[]; onChange: (g: GrantInput[]) => void }) {
  const spaces = useSpaces()
  const domains = [{ value: '*', label: 'All spaces (global)' }, ...(spaces.data ?? []).map((s) => ({ value: s.id, label: s.name }))]
  const set = (i: number, g: Partial<GrantInput>) => onChange(value.map((x, j) => (j === i ? { ...x, ...g } : x)))
  return (
    <Stack gap={6}>
      <Text size="sm" fw={500}>Grants</Text>
      {value.map((g, i) => (
        <Group key={i} gap="xs" wrap="nowrap">
          <Select aria-label="Role" w={130} allowDeselect={false} data={ROLES} value={g.role} onChange={(v) => v && set(i, { role: v as Role })} />
          <Select aria-label="Scope" flex={1} allowDeselect={false} searchable data={domains} value={g.domain} onChange={(v) => v && set(i, { domain: v })} />
          <ActionIcon aria-label="Remove grant" onClick={() => onChange(value.filter((_, j) => j !== i))}><IconTrash size={14} /></ActionIcon>
        </Group>
      ))}
      <Button variant="subtle" size="compact-xs" w="fit-content" leftSection={<IconPlus size={12} />} onClick={() => onChange([...value, { role: 'viewer', domain: '*' }])}>Add grant</Button>
      <Text size="xs" c="dimmed">Operator and admin grants require multi-factor authentication.</Text>
    </Stack>
  )
}
