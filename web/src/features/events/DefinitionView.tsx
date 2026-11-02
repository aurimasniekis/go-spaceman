import { Badge, Code, Group, Stack, Table, Text } from '@mantine/core'
import type { Definition } from '@/api/client'

function Field({ label, value }: { label: string; value?: string }) {
  if (!value) return null
  return (
    <Stack gap={2}>
      <Text size="xs" c="dimmed" tt="uppercase" fw={600}>{label}</Text>
      <Code block className="mono" style={{ whiteSpace: 'pre-wrap' }}>{value}</Code>
    </Stack>
  )
}

/** A template definition, read-only. */
export function DefinitionView({ def }: { def: Definition }) {
  return (
    <Stack gap="sm">
      <Field label="Subject" value={def.subject} />
      {(def.variables ?? []).length > 0 && (
        <Stack gap={2}>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Variables</Text>
          <Table withTableBorder>
            <Table.Thead><Table.Tr><Table.Th>Name</Table.Th><Table.Th>Pattern</Table.Th><Table.Th>Description</Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {def.variables.map((v) => (
                <Table.Tr key={v.name}>
                  <Table.Td className="mono">{v.name}{v.required && <Badge ml={6} size="xs" variant="outline" color="gray">required</Badge>}</Table.Td>
                  <Table.Td className="mono">{v.pattern || '—'}</Table.Td>
                  <Table.Td><Text size="xs">{v.description}{v.example ? ` (e.g. ${v.example})` : ''}</Text></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Stack>
      )}
      {(def.filters ?? []).length > 0 && (
        <Stack gap={2}>
          <Text size="xs" c="dimmed" tt="uppercase" fw={600}>Filters</Text>
          <Group gap={6}>
            {def.filters.map((f, i) => (
              <Code key={i} className="mono">{f.path} {f.op} {f.op === 'in' ? (f.values ?? []).join(', ') : f.value}</Code>
            ))}
          </Group>
        </Stack>
      )}
      <Field label="Title" value={def.title} />
      <Field label="Body" value={def.body} />
      <Field label="Instructions for control" value={def.instructions} />
      <Field label="Correlation" value={def.correlation} />
      {def.input_schema != null && <Field label="Input schema" value={JSON.stringify(def.input_schema, null, 2)} />}
      {def.urgency && <Text size="sm">Urgency: <Badge variant="light">{def.urgency}</Badge></Text>}
    </Stack>
  )
}
