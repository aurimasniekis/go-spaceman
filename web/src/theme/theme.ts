import { createTheme, defaultVariantColorsResolver, type CSSVariablesResolver, type MantineColorsTuple, type VariantColorsResolver } from '@mantine/core'
import { lightOwl, nightOwl } from './nightowl'

// Ten-step tuples built around the Night Owl accents. Index 5 is the dark filled shade and 6 its hover; 7 is the light
// filled shade and 8 its hover. Each is checked for WCAG AA against its text color in contrast.test.ts.
export const owlBlue: MantineColorsTuple = ['#eef3ff', '#dce6ff', '#b6c9ff', '#94b3ff', '#82aaff', '#82aaff', '#5f88e0', '#3a63bd', '#2f52a0', '#243f80']
export const owlPurple: MantineColorsTuple = ['#f8efff', '#efdcfb', '#ddb7f2', '#cf96ec', '#c792ea', '#c792ea', '#b46fdb', '#8340aa', '#6c348c', '#56296f']
export const owlGreen: MantineColorsTuple = ['#f3fbe6', '#e6f6cc', '#cfec9b', '#bde47b', '#addb67', '#addb67', '#08916a', '#066b4e', '#06644a', '#044d39']
export const owlRed: MantineColorsTuple = ['#ffebeb', '#fdd5d5', '#f6a8a7', '#f27b79', '#f2726f', '#f2726f', '#ec6563', '#c22f2d', '#a32523', '#821b1a']
export const owlYellow: MantineColorsTuple = ['#fff8eb', '#fdecd0', '#f7d9a4', '#f1c98a', '#ecc48d', '#ecc48d', '#b48a31', '#7f5f22', '#7f5f22', '#644b1a']
// Dark surface scale (Mantine "dark" colors): 7 is the body background.
const owlDark: MantineColorsTuple = ['#d6deeb', '#b2bccc', '#8d9aad', '#637777', '#2a4a63', '#1d3b53', '#0b2942', '#011627', '#01111e', '#000c16']

/**
 * Night Owl's accents are light, so filled controls take the scheme's --sm-on-filled text color (dark on Night Owl, white
 * on Light Owl); white on #82aaff would be ~2.4:1 (SM-UI-006). Mantine's autoContrast cannot do this: it ignores the
 * active color scheme.
 */
const filledText: VariantColorsResolver = (input) => {
  const r = defaultVariantColorsResolver(input)
  return input.variant === 'filled' ? { ...r, color: 'var(--sm-on-filled)' } : r
}

export const theme = createTheme({
  primaryColor: 'owlBlue',
  // Light Owl: shade 7 keeps white on filled controls and light-variant text above 4.5:1 (shade 6 is 4.3:1).
  primaryShade: { light: 7, dark: 5 },
  variantColorResolver: (input) => filledText(input),
  colors: { owlBlue, owlPurple, owlGreen, owlRed, owlYellow, dark: owlDark },
  fontFamily: "'Inter Variable', Inter, system-ui, -apple-system, 'Segoe UI', sans-serif",
  fontFamilyMonospace: "'JetBrains Mono Variable', 'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, monospace",
  headings: { fontFamily: "'Inter Variable', Inter, system-ui, sans-serif", fontWeight: '600' },
  defaultRadius: 'sm',
  radius: { xs: '2px', sm: '4px', md: '6px', lg: '8px', xl: '10px' },
  // sm 13 / xs 12: field descriptions and table hints were 11 px in dimmed grey, too small to read comfortably.
  fontSizes: { xs: '12px', sm: '13px', md: '14px', lg: '16px', xl: '20px' },
  spacing: { xs: '6px', sm: '8px', md: '12px', lg: '16px', xl: '24px' },
  cursorType: 'pointer',
  components: {
    // Descriptions go under the field, not between the label and the field, so inputs in one row stay aligned whether or
    // not they have a description. The error comes last, under the description.
    InputWrapper: { defaultProps: { inputWrapperOrder: ['label', 'input', 'description', 'error'] } },
    Button: { defaultProps: { size: 'xs' } },
    // 28 px row actions: 22 px was under the WCAG 2.5.8 target size.
    ActionIcon: { defaultProps: { size: 'md', variant: 'subtle', color: 'gray' } },
    TextInput: { defaultProps: { size: 'xs' } },
    PasswordInput: { defaultProps: { size: 'xs' } },
    NumberInput: { defaultProps: { size: 'xs' } },
    Select: { defaultProps: { size: 'xs' } },
    Textarea: { defaultProps: { size: 'xs' } },
    SegmentedControl: { defaultProps: { size: 'xs' } },
    // States and categories in sentence case: uppercase made every badge shout, names and titles included.
    Badge: { defaultProps: { size: 'sm', radius: 'sm', variant: 'light' }, styles: { root: { textTransform: 'none', fontWeight: 600, letterSpacing: 0 } } },
    Table: { defaultProps: { fz: 'sm', verticalSpacing: 6, horizontalSpacing: 'sm', highlightOnHover: true } },
    Card: { defaultProps: { withBorder: true, padding: 'md', radius: 'md' } },
    Tooltip: { defaultProps: { openDelay: 300, fz: 'xs' } },
    NavLink: { defaultProps: { py: 5 } },
    // Close buttons have no visible text; give them a name (axe button-name).
    // Dialogs scroll vertically only. Mantine's content box sets overflow-y: auto, and CSS then makes overflow-x auto as
    // well, so anything a pixel wider than the dialog (or a node a browser extension adds next to the editor) gave the
    // dialog a horizontal scrollbar and, with it, a vertical one. Long unbreakable values (IDs, fingerprints, URLs) wrap;
    // wide content (tables, code) scrolls in its own box; e2e/overflow.spec.ts fails when a dialog's content is wider
    // than the dialog.
    Modal: {
      defaultProps: { closeButtonProps: { 'aria-label': 'Close dialog' } },
      styles: { title: { fontWeight: 600, fontSize: 'var(--mantine-font-size-lg)' }, content: { overflowX: 'hidden' }, body: { overflowWrap: 'break-word' } },
    },
    Drawer: {
      defaultProps: { closeButtonProps: { 'aria-label': 'Close panel' } },
      styles: { title: { fontWeight: 600, fontSize: 'var(--mantine-font-size-lg)' }, content: { overflowX: 'hidden' }, body: { overflowWrap: 'break-word' } },
    },
  },
})

/**
 * Night Owl variant colors for the `light` variant (badges, alerts, light buttons). Mantine's dark scheme takes the text
 * from shade 0 (near-white) and a 15 % tint of shade 3, so every hue looked the same grey: READY, WAITING and a warning
 * alert all read alike. The accent itself (shade 4) carries the text instead, on a 12 % tint of it (18 % on hover); red stays above 4.5:1 only up to about 12 %.
 */
export const ACCENTS = { owlBlue, owlPurple, owlGreen, owlRed, owlYellow }

function rgba(hex: string, a: number): string {
  const [r, g, b] = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16))
  return `rgba(${r}, ${g}, ${b}, ${a})`
}

function darkLightVariants(): Record<string, string> {
  const vars: Record<string, string> = {}
  for (const [name, c] of Object.entries(ACCENTS)) {
    vars[`--mantine-color-${name}-light-color`] = c[4]
    vars[`--mantine-color-${name}-light`] = rgba(c[4], 0.12)
    vars[`--mantine-color-${name}-light-hover`] = rgba(c[4], 0.18)
  }
  return vars
}

export const LIGHT_DIMMED = '#5c6670'
// Readable on the hovered or selected row too (#1d3b53, 4.6:1), not only on the body and cards.
export const DARK_DIMMED = '#93a6b4'

/** Exact Night Owl / Light Owl surfaces as CSS variables (body, borders, dimmed text). */
export const cssVariablesResolver: CSSVariablesResolver = () => ({
  variables: { '--sm-mono': 'var(--mantine-font-family-monospace)' },
  light: {
    '--mantine-color-body': lightOwl.bg,
    '--mantine-color-text': lightOwl.fg,
    '--mantine-color-default-border': lightOwl.border,
    // Mantine's default dimmed gray is under 4.5:1 on Light Owl surfaces (WCAG AA, see contrast.test.ts).
    '--mantine-color-dimmed': LIGHT_DIMMED,
    '--sm-surface': '#ffffff',
    '--sm-on-filled': '#ffffff',
    '--sm-accent-purple': lightOwl.purple,
    '--sm-accent-teal': lightOwl.teal,
  },
  dark: {
    '--mantine-color-body': nightOwl.bg,
    '--mantine-color-text': nightOwl.fg,
    '--mantine-color-default-border': nightOwl.border,
    '--mantine-color-dimmed': DARK_DIMMED,
    '--sm-surface': nightOwl.bgRaised,
    '--sm-on-filled': nightOwl.bg,
    '--sm-accent-purple': nightOwl.purple,
    '--sm-accent-teal': nightOwl.teal,
    ...darkLightVariants(),
  },
})
