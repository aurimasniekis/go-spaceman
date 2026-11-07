import { Accordion, ActionIcon, Badge, Button, Group, Select, Stack, Switch, Text, Textarea, TextInput } from '@mantine/core'
import { IconPlus, IconTrash } from '@tabler/icons-react'
import { type AutoList, type ClaudeSettingsForm, PERMISSION_MODES } from './claudeSettings'

/**
 * A claude-code profile's Claude Code settings (ADR-0059): settings.json keys set in each run's own settings.json.
 * Permissions, auto mode and environment variables have editors; any other key is JSON under "Other settings".
 */
export function ClaudeSettingsEditor({ value, onChange, error }: { value: ClaudeSettingsForm; onChange: (v: ClaudeSettingsForm) => void; error?: string }) {
  const set = <K extends keyof ClaudeSettingsForm>(k: K, v: ClaudeSettingsForm[K]) => onChange({ ...value, [k]: v })
  const permCount = value.permAllow.length + value.permAsk.length + value.permDeny.length + value.directories.length + (value.defaultMode ? 1 : 0)
  const autoCount = value.environment.rules.length + value.allow.rules.length + value.softDeny.rules.length
  return (
    <Stack gap={4}>
      <Text size="sm" fw={500}>Claude Code settings</Text>
      <Text size="xs" c="dimmed">Set in each run's own settings.json at launch; keys removed here are removed at the next launch. Nodes need SpaceMan 0.1.0-rc.9 or later.</Text>
      <Accordion variant="contained" multiple>
        <Accordion.Item value="permissions">
          <Accordion.Control><Title label="Permissions" count={permCount} /></Accordion.Control>
          <Accordion.Panel>
            <Stack gap="md">
              <Select label="Default permission mode" placeholder="Not set" clearable value={value.defaultMode || null} onChange={(m) => set('defaultMode', m ?? '')}
                data={PERMISSION_MODES.map((m) => ({ value: m, label: m }))} classNames={{ input: 'mono' }}
                description="Lowered to the central ceiling (harness.max_permission_mode). The permission_mode setting, when set, wins." />
              <Text size="xs" c="dimmed">Tool rules such as Bash(git push:*) or Read(./secrets/**). Deny wins over ask, ask over allow.</Text>
              <Rules label="Allow" description="Used without asking." placeholder="Bash(make test:*)" value={value.permAllow} onChange={(v) => set('permAllow', v)} />
              <Rules label="Ask" description="Always asks first." placeholder="Bash(git push:*)" value={value.permAsk} onChange={(v) => set('permAsk', v)} />
              <Rules label="Deny" description="Refused." placeholder="Read(./secrets/**)" value={value.permDeny} onChange={(v) => set('permDeny', v)} />
              <Rules label="Additional directories" description="Folders outside the working directory the run may use." placeholder="/data/shared"
                value={value.directories} onChange={(v) => set('directories', v)} />
            </Stack>
          </Accordion.Panel>
        </Accordion.Item>
        <Accordion.Item value="auto">
          <Accordion.Control><Title label="Auto mode rules" count={autoCount} /></Accordion.Control>
          <Accordion.Panel>
            <Stack gap="md">
              <Text size="xs" c="dimmed">Used when the permission mode is auto. Each rule is a sentence the auto mode classifier reads.</Text>
              <AutoRules label="Environment" description="What the classifier may trust: hosts, repositories, services and how they are reached."
                placeholder="Source control: GitHub repositories of example-org, reached through the gateway host." value={value.environment} onChange={(v) => set('environment', v)} />
              <AutoRules label="Allowed actions" description="Exceptions to the block rules."
                placeholder="Pushing branches to the space's repositories is allowed." value={value.allow} onChange={(v) => set('allow', v)} />
              <AutoRules label="Block rules" description="More actions to block."
                placeholder="Deleting or moving release tags." value={value.softDeny} onChange={(v) => set('softDeny', v)} />
            </Stack>
          </Accordion.Panel>
        </Accordion.Item>
        <Accordion.Item value="env">
          <Accordion.Control><Title label="Environment variables" count={value.env.length} /></Accordion.Control>
          <Accordion.Panel>
            <Stack gap={4}>
              <Text size="xs" c="dimmed">Set for the Claude Code session. Not for secrets: settings are stored in clear and recorded in each run's manifest.</Text>
              {value.env.map(([k, v], i) => (
                <Group key={i} gap="xs" wrap="nowrap">
                  <TextInput aria-label={`Variable ${i + 1} name`} placeholder="NAME" classNames={{ input: 'mono' }} value={k} style={{ flex: 1 }}
                    onChange={(e) => set('env', value.env.map((p, j) => (j === i ? [e.target.value, p[1]] : p)))} />
                  <TextInput aria-label={`Variable ${i + 1} value`} placeholder="value" classNames={{ input: 'mono' }} value={v} style={{ flex: 2 }}
                    onChange={(e) => set('env', value.env.map((p, j) => (j === i ? [p[0], e.target.value] : p)))} />
                  <ActionIcon aria-label={`Remove variable ${k || i + 1}`} onClick={() => set('env', value.env.filter((_, j) => j !== i))}><IconTrash size={14} /></ActionIcon>
                </Group>
              ))}
              <Button size="xs" variant="subtle" leftSection={<IconPlus size={12} />} style={{ alignSelf: 'flex-start' }}
                onClick={() => set('env', [...value.env, ['', '']])} disabled={value.env.some(([k]) => k === '')}>Add variable</Button>
            </Stack>
          </Accordion.Panel>
        </Accordion.Item>
        <Accordion.Item value="other">
          <Accordion.Control><Title label="Other settings" count={value.other.trim() ? 1 : 0} badge="JSON" /></Accordion.Control>
          <Accordion.Panel>
            <Textarea label="Other settings.json keys" description="Any other Claude Code settings as a JSON object, such as hooks, statusLine or model. Hooks run commands on the node."
              placeholder={'{\n  "hooks": { … }\n}'} autosize minRows={4} maxRows={20} classNames={{ input: 'mono' }} value={value.other}
              onChange={(e) => set('other', e.target.value)} error={error} spellCheck={false} />
          </Accordion.Panel>
        </Accordion.Item>
      </Accordion>
    </Stack>
  )
}

function Title({ label, count, badge }: { label: string; count: number; badge?: string }) {
  return <Group gap="xs">{label}{count > 0 && <Badge size="xs" variant="light">{badge ?? count}</Badge>}</Group>
}

function AutoRules({ label, description, placeholder, value, onChange }: { label: string; description: string; placeholder: string; value: AutoList; onChange: (v: AutoList) => void }) {
  return (
    <Stack gap={4}>
      <Rules label={label} description={description} placeholder={placeholder} multiline value={value.rules} onChange={(rules) => onChange({ ...value, rules })} />
      {value.rules.length > 0 && (
        <Switch size="xs" label="Replace Claude Code's default rules" description={value.replace ? 'Only these rules apply.' : "These rules are added to Claude Code's defaults."}
          checked={value.replace} onChange={(e) => onChange({ ...value, replace: e.currentTarget.checked })} />
      )}
    </Stack>
  )
}

function Rules({ label, description, placeholder, multiline, value, onChange }: {
  label: string; description: string; placeholder: string; multiline?: boolean; value: string[]; onChange: (v: string[]) => void
}) {
  const put = (i: number, v: string) => onChange(value.map((r, j) => (j === i ? v : r)))
  return (
    <Stack gap={4}>
      <div>
        <Text size="sm">{label}</Text>
        <Text size="xs" c="dimmed">{description}</Text>
      </div>
      {value.map((r, i) => (
        <Group key={i} gap="xs" wrap="nowrap" align="flex-start">
          {multiline
            ? <Textarea aria-label={`${label} rule ${i + 1}`} placeholder={placeholder} autosize minRows={1} value={r} onChange={(e) => put(i, e.target.value.replace(/\n/g, ' '))} style={{ flex: 1 }} />
            : <TextInput aria-label={`${label} rule ${i + 1}`} placeholder={placeholder} classNames={{ input: 'mono' }} value={r} onChange={(e) => put(i, e.target.value)} style={{ flex: 1 }} />}
          <ActionIcon mt={6} aria-label={`Remove ${label.toLowerCase()} rule ${i + 1}`} onClick={() => onChange(value.filter((_, j) => j !== i))}><IconTrash size={14} /></ActionIcon>
        </Group>
      ))}
      <Button size="xs" variant="subtle" leftSection={<IconPlus size={12} />} style={{ alignSelf: 'flex-start' }} aria-label={`Add rule to ${label}`}
        onClick={() => onChange([...value, ''])} disabled={value.includes('')}>Add rule</Button>
    </Stack>
  )
}
