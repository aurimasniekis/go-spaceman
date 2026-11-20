import { useMemo, useState } from 'react'
import { useSpaceAccounts } from '@/api/queries'
import { UsageExplorer } from './UsageExplorer'
import type { UsageSearch } from './state'

/**
 * A space's token usage from the usage ledger (AS-T7): the usage explorer fixed to one space. Its state is local here;
 * the all-spaces page keeps the same state in the URL.
 */
export function SpaceUsage({ spaceId }: { spaceId: string }) {
  const [search, setSearch] = useState<UsageSearch>({})
  const accounts = useSpaceAccounts(spaceId)
  const names = useMemo(() => Object.fromEntries((accounts.data ?? []).map((a) => [a.account_id, a.name ?? ''])), [accounts.data])
  return <UsageExplorer spaceId={spaceId} search={search} onSearch={setSearch} names={names} />
}
