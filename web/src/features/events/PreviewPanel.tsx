import { Badge, Button, Card, Group, JsonInput, SimpleGrid, Stack, Text, TextInput } from '@mantine/core'
import { IconFlask } from '@tabler/icons-react'
import { useForm } from '@tanstack/react-form'
import { useState } from 'react'
import { api, unwrap, type Definition, type Schemas } from '@/api/client'
import { errorMessage } from '@/components/ErrorAlert'
import { MarkdownView } from '@/components/MarkdownView'
import { fieldError } from '@/lib/form'
import { samples } from './samples'

type Preview = Schemas['PreviewOut']

const outcomeColor: Record<Preview['outcome'], string> = { accepted: 'owlGreen', filtered: 'gray', quarantined: 'owlRed' }

const jsonError = ({ value }: { value: string }) => {
  try {
    JSON.parse(value)
    return undefined
  } catch {
    return 'The sample is not valid JSON'
  }
}

/**
 * Tests a definition against a sample event (SM-EVT-017): the effective subject, whether the sample would be accepted,
 * filtered or quarantined, and the inbox item it would become. Nothing is stored or subscribed. The server validates
 * and binds the definition first, so its validation errors show here too.
 *
 * With `vars`, the caller owns the variable values (a subscription form) and the panel shows no variable inputs.
 */
export function PreviewPanel({ def, slug, initialVars, vars: fixedVars }: { def: Definition; slug?: string; initialVars?: Record<string, string>; vars?: Record<string, string> }) {
  const sample = slug ? samples[slug] : undefined
  const [result, setResult] = useState<Preview | null>(null)
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: {
      vars: Object.fromEntries((def.variables ?? []).map((x) => [x.name, initialVars?.[x.name] ?? x.example ?? x.default ?? ''])) as Record<string, string>,
      subject: sample?.subject ?? '',
      type: sample?.type ?? '',
      data: sample ? JSON.stringify(sample.data, null, 2) : '{}',
    },
    onSubmit: async ({ value }) => {
      setError('')
      try {
        setResult(unwrap(await api.POST('/event-templates/preview', {
          body: { definition: def, vars: fixedVars ?? value.vars, event: { subject: value.subject, type: value.type, source: sample?.source, data: JSON.parse(value.data) } },
        })))
      } catch (e) {
        setResult(null)
        setError(errorMessage(e))
      }
    },
  })

  return (
    <Stack gap="sm">
      {!fixedVars && (def.variables ?? []).length > 0 && (
        <SimpleGrid cols={{ base: 1, sm: 2 }}>
          {def.variables.map((v) => (
            <form.Field key={v.name} name={`vars.${v.name}` as `vars.${string}`}>
              {(f) => <TextInput label={v.name} description={v.description} classNames={{ input: 'mono' }} value={f.state.value ?? ''} onChange={(e) => f.handleChange(e.target.value)} />}
            </form.Field>
          ))}
        </SimpleGrid>
      )}
      <SimpleGrid cols={{ base: 1, sm: 2 }}>
        <form.Field name="subject">
          {(f) => <TextInput label="Sample subject" classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
        </form.Field>
        <form.Field name="type">
          {(f) => <TextInput label="Sample type" classNames={{ input: 'mono' }} value={f.state.value} onChange={(e) => f.handleChange(e.target.value)} />}
        </form.Field>
      </SimpleGrid>
      <form.Field name="data" validators={{ onChange: jsonError, onSubmit: jsonError }}>
        {(f) => <JsonInput label="Sample payload" description={sample ? 'Pre-filled with the go-pulse-github issues_opened event' : undefined}
          autosize minRows={6} maxRows={16} classNames={{ input: 'mono' }} value={f.state.value} onBlur={f.handleBlur} onChange={f.handleChange} error={fieldError(f.state.meta)} />}
      </form.Field>
      <Group>
        <form.Subscribe selector={(s) => s.isSubmitting}>
          {(busy) => <Button leftSection={<IconFlask size={14} />} loading={busy} onClick={() => void form.handleSubmit()}>Test</Button>}
        </form.Subscribe>
      </Group>
      {error && <Text size="sm" c="owlRed" role="alert" style={{ whiteSpace: 'pre-wrap' }}>{error}</Text>}
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
