import { Badge, Button, Card, Group, JsonInput, SimpleGrid, Stack, Text, TextInput } from '@mantine/core'
import { IconFlask } from '@tabler/icons-react'
import { useState } from 'react'
import { api, unwrap, type Definition, type Schemas } from '@/api/client'
import { errorMessage } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import { samples } from './samples'

type Preview = Schemas['PreviewOut']

const outcomeColor: Record<Preview['outcome'], string> = { accepted: 'owlGreen', filtered: 'gray', quarantined: 'owlRed' }

/**
 * Tests a definition against a sample event (SM-EVT-017): the effective subject, whether the sample would be accepted,
 * filtered or quarantined, and the inbox item it would become. Nothing is stored or subscribed.
 */
export function PreviewPanel({ def, slug, initialVars }: { def: Definition; slug?: string; initialVars?: Record<string, string> }) {
  const sample = slug ? samples[slug] : undefined
  const [vars, setVars] = useState<Record<string, string>>(() => {
    const v: Record<string, string> = {}
    for (const x of def.variables ?? []) v[x.name] = initialVars?.[x.name] ?? x.example ?? x.default ?? ''
    return v
  })
  const [subject, setSubject] = useState(sample?.subject ?? '')
  const [type, setType] = useState(sample?.type ?? '')
  const [data, setData] = useState(sample ? JSON.stringify(sample.data, null, 2) : '{}')
  const [result, setResult] = useState<Preview | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const run = async () => {
    setError('')
    setBusy(true)
    try {
      setResult(unwrap(await api.POST('/event-templates/preview', {
        body: { definition: def, vars, event: { subject, type, source: sample?.source, data: JSON.parse(data) } },
      })))
    } catch (e) {
      setResult(null)
      setError(e instanceof SyntaxError ? 'The sample is not valid JSON' : errorMessage(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <Stack gap="sm">
      {(def.variables ?? []).length > 0 && (
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          {def.variables.map((v) => (
            <TextInput key={v.name} label={v.name} description={v.description} classNames={{ input: "mono" }} value={vars[v.name] ?? ''}
              onChange={(e) => setVars({ ...vars, [v.name]: e.target.value })} />
          ))}
        </SimpleGrid>
      )}
      <SimpleGrid cols={{ base: 1, sm: 2 }}>
        <TextInput label="Sample subject" classNames={{ input: 'mono' }} value={subject} onChange={(e) => setSubject(e.target.value)} />
        <TextInput label="Sample type" classNames={{ input: 'mono' }} value={type} onChange={(e) => setType(e.target.value)} />
      </SimpleGrid>
      <JsonInput label="Sample payload" description={sample ? 'Pre-filled with the go-pulse-github issues_opened event' : undefined}
        autosize minRows={6} maxRows={16} classNames={{ input: 'mono' }} value={data} onChange={setData} />
      <Group><Button leftSection={<IconFlask size={14} />} loading={busy} onClick={() => void run()}>Test</Button></Group>
      {error && <Text size="sm" c="red" role="alert">{error}</Text>}
      {result && (
        <Card>
          <Stack gap="xs">
            <Group gap="xs">
              <Badge color={outcomeColor[result.outcome]}>{result.outcome}</Badge>
              <Text size="xs" c="dimmed">filter <span className="mono">{result.subject}</span> {result.subject_match ? 'matches' : 'does not match'} the sample subject</Text>
            </Group>
            {result.reason && <Text size="sm">{result.reason}</Text>}
            {result.outcome === 'accepted' && (
              <>
                <Text fw={600}>{result.title}</Text>
                <MarkdownView source={result.body ?? ''} spaceId="" />
                {result.instructions && <Text size="xs" c="dimmed">Instructions for control: {result.instructions}</Text>}
                {result.correlation && <Text size="xs" c="dimmed">Correlation: <span className="mono">{result.correlation}</span></Text>}
              </>
            )}
          </Stack>
        </Card>
      )}
    </Stack>
  )
}
