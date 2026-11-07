import { useMediaQuery } from '@mantine/hooks'

/** Below Mantine's `sm` breakpoint (48em, 768 px): phones and narrow windows. The app shell's navbar uses the same one. */
export const NARROW_QUERY = '(max-width: 47.99em)'

/** Whether the viewport is phone-sized, read on the first render so a phone never flashes the desktop layout. */
export function useNarrow(): boolean {
  return useMediaQuery(NARROW_QUERY, undefined, { getInitialValueInEffect: false }) ?? false
}
