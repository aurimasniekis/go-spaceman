/** The SpaceMan mark: an orbit around a terminal cursor. */
export function Logo({ size = 18 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <ellipse cx="12" cy="12" rx="10" ry="4.5" transform="rotate(-25 12 12)" stroke="var(--sm-accent-purple)" strokeWidth="1.6" />
      <rect x="9" y="8" width="6" height="8" rx="1" fill="var(--mantine-primary-color-filled)" />
    </svg>
  )
}
