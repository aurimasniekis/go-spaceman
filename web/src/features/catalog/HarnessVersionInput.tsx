import { Autocomplete } from '@mantine/core'
import { HARNESS_CHANNELS, HARNESS_VERSION_HINT } from './harnessVersion'

/** The profile form's Claude Code version pin: free text with the channels offered, and the hint. */
export function HarnessVersionInput({ value, onChange, error }: { value: string; onChange: (v: string) => void; error?: string }) {
  return (
    <Autocomplete label="Claude Code version" placeholder="Node default" description={HARNESS_VERSION_HINT} classNames={{ input: 'mono' }}
      data={[...HARNESS_CHANNELS]} value={value} onChange={onChange} error={error || undefined} />
  )
}
