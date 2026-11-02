import { Badge, Button, Group, Modal, Select, Stack, Table, Text, TextInput } from '@mantine/core'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { api, unwrap } from '@/api/client'
import { keys, useSpaceHumanTypes } from '@/api/queries'
import { ErrorAlert } from '@/components/ErrorAlert'
import { notifyError } from '@/lib/notify'

/** Enable catalog types in a space, following the latest version or pinned, with an optional label (SM-HTYPE-002). */
export function SpaceTypesModal({ spaceId, onClose }: { spaceId: string; onClose: () => void }) {
  const qc = useQueryClient()
  const enabled = useSpaceHumanTypes(spaceId).data ?? []
  const options = useQuery({ queryKey: ['human-type-options', spaceId],
    queryFn: async () => unwrap(await api.GET('/spaces/{space_id}/human-type-options', { params: { path: { space_id: spaceId } } })).items })
  const [labels, setLabels] = useState<Record<string, string>>({})
  const [pins, setPins] = useState<Record<string, string>>({})
  const refresh = () => void qc.invalidateQueries({ queryKey: keys.spaceHumanTypes(spaceId) })
  const enable = async (typeId: string) => {
    try {
      unwrap(await api.PUT('/spaces/{space_id}/human-types/{type_id}', { params: { path: { space_id: spaceId, type_id: typeId } },
        body: { label: labels[typeId] || undefined, version_pin: pins[typeId] || undefined } }))
      refresh()
    } catch (e) {
      notifyError(e, 'Could not enable the type')
    }
  }
  const disable = async (typeId: string) => {
    try {
      const r = await api.DELETE('/spaces/{space_id}/human-types/{type_id}', { params: { path: { space_id: spaceId, type_id: typeId } } })
      if (r.error) throw r.error
      refresh()
    } catch (e) {
      notifyError(e, 'Could not disable the type')
    }
  }
  return (
    <Modal opened onClose={onClose} title="Human action types in this space" size="xl">
      <Stack gap="sm">
        <Text size="sm" c="dimmed">Enabled types can be asked by people and agents here. Disabling a type stops new requests; outstanding ones are unaffected.</Text>
        <ErrorAlert error={options.error} />
        <Table>
          <Table.Thead><Table.Tr><Table.Th>Type</Table.Th><Table.Th>Label in this space</Table.Th><Table.Th>Version</Table.Th><Table.Th /></Table.Tr></Table.Thead>
          <Table.Tbody>
            {(options.data ?? []).map((o) => {
              const on = enabled.find((e) => e.type_id === o.id)
              return (
                <Table.Tr key={o.id}>
                  <Table.Td><Text size="sm" fw={500}>{o.name}</Text>{on && <Badge size="xs" color="owlGreen">enabled · v{on.number}</Badge>}</Table.Td>
                  <Table.Td><TextInput aria-label={`Label for ${o.name}`} placeholder={on?.label ?? o.name} value={labels[o.id] ?? ''} onChange={(e) => setLabels({ ...labels, [o.id]: e.target.value })} /></Table.Td>
                  <Table.Td>
                    <Select aria-label={`Version of ${o.name}`} allowDeselect={false} value={pins[o.id] ?? 'follow'} onChange={(v) => setPins({ ...pins, [o.id]: v === 'follow' || !v ? '' : v })}
                      data={[{ value: 'follow', label: 'Latest' }, ...o.versions.map((v) => ({ value: v.id, label: `v${v.number}` }))]} />
                  </Table.Td>
                  <Table.Td>
                    <Group gap={4} justify="flex-end" wrap="nowrap">
                      <Button size="xs" variant={on ? 'default' : 'filled'} onClick={() => void enable(o.id)}>{on ? 'Update' : 'Enable'}</Button>
                      {on && <Button size="xs" variant="subtle" color="owlRed" onClick={() => void disable(o.id)}>Disable</Button>}
                    </Group>
                  </Table.Td>
                </Table.Tr>
              )
            })}
          </Table.Tbody>
        </Table>
        {options.isSuccess && options.data.length === 0 && <Text size="sm" c="dimmed">No published custom types yet.</Text>}
      </Stack>
    </Modal>
  )
}
