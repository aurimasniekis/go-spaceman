import { ActionIcon, Button, Group, Stack, Text, TextInput } from '@mantine/core'
import { IconPlus, IconTrash } from '@tabler/icons-react'

/** Key/value harness settings (e.g. permission_mode). Later layers override earlier ones key by key. */
export function SettingsEditor({ value, onChange }: { value: Record<string, string>; onChange: (v: Record<string, string>) => void }) {
  const rows = Object.entries(value)
  const set = (i: number, k: string, v: string) => {
    const next = rows.map((r, j) => (j === i ? [k, v] : r))
    onChange(Object.fromEntries(next))
  }
  return (
    <Stack gap={4}>
      <Text size="sm" fw={500}>Settings</Text>
      {rows.map(([k, v], i) => (
        <Group key={i} gap="xs" wrap="nowrap">
          <TextInput aria-label={`Setting ${i + 1} name`} placeholder="permission_mode" classNames={{ input: 'mono' }} value={k} onChange={(e) => set(i, e.target.value, v)} style={{ flex: 1 }} />
          <TextInput aria-label={`Setting ${i + 1} value`} placeholder="acceptEdits" classNames={{ input: 'mono' }} value={v} onChange={(e) => set(i, k, e.target.value)} style={{ flex: 1 }} />
          <ActionIcon aria-label={`Remove setting ${k || i + 1}`} onClick={() => onChange(Object.fromEntries(rows.filter((_, j) => j !== i)))}><IconTrash size={14} /></ActionIcon>
        </Group>
      ))}
      <Button size="xs" variant="subtle" leftSection={<IconPlus size={12} />} style={{ alignSelf: 'flex-start' }}
        onClick={() => onChange({ ...value, '': '' })} disabled={'' in value}>Add setting</Button>
    </Stack>
  )
}
