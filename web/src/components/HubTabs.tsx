import { Group, ScrollArea } from '@mantine/core'
import { Link, useRouterState } from '@tanstack/react-router'
import { useMe } from '@/api/queries'
import { hubOf, onPath, visibleTabs, type HubKey } from '@/lib/nav'
import classes from './HubTabs.module.css'

/**
 * The tabs of a settings hub, shown under the header of each member page. They are plain links (keyboard, middle-click
 * and open-in-new-tab work), and the current page carries `aria-current="page"`. Only tabs the user may see are listed;
 * a hub with one visible tab shows none.
 */
export function HubTabs({ hub }: { hub: HubKey }) {
  const me = useMe().data
  const path = useRouterState({ select: (st) => st.location.pathname })
  const h = hubOf(hub)
  const tabs = visibleTabs(h, me)
  if (tabs.length < 2) return null
  return (
    <ScrollArea type="auto" scrollbarSize={6} mb="md" offsetScrollbars="x">
      <Group component="nav" aria-label={`${h.label} sections`} gap={4} wrap="nowrap" className={classes.tabs}>
        {tabs.map((t) => (
          <Link key={t.to} to={t.to} className={classes.tab} aria-current={onPath(path, t.to) ? 'page' : undefined}>
            {t.label}
          </Link>
        ))}
      </Group>
    </ScrollArea>
  )
}
