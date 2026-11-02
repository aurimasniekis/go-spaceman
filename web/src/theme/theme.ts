import { createTheme, defaultVariantColorsResolver, type CSSVariablesResolver, type MantineColorsTuple, type VariantColorsResolver } from '@mantine/core'
import { lightOwl, nightOwl } from './nightowl'

// Ten-step tuples built around the Night Owl accents (index 5 dark, 6 light are the primary shades).
const owlBlue: MantineColorsTuple = ['#eef3ff', '#dce6ff', '#b6c9ff', '#94b3ff', '#82aaff', '#82aaff', '#4876d6', '#3a63bd', '#2f52a0', '#243f80']
const owlPurple: MantineColorsTuple = ['#f8efff', '#efdcfb', '#ddb7f2', '#cf96ec', '#c792ea', '#c792ea', '#994cc3', '#8340aa', '#6c348c', '#56296f']
const owlGreen: MantineColorsTuple = ['#f3fbe6', '#e6f6cc', '#cfec9b', '#bde47b', '#addb67', '#addb67', '#08916a', '#077b5a', '#06644a', '#044d39']
const owlRed: MantineColorsTuple = ['#ffebeb', '#fdd5d5', '#f6a8a7', '#f27b79', '#ef5350', '#ef5350', '#de3d3b', '#c22f2d', '#a32523', '#821b1a']
const owlYellow: MantineColorsTuple = ['#fff8eb', '#fdecd0', '#f7d9a4', '#f1c98a', '#ecc48d', '#ecc48d', '#b48a31', '#9a7429', '#7f5f22', '#644b1a']
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
  primaryShade: { light: 6, dark: 5 },
  variantColorResolver: (input) => filledText(input),
  colors: { owlBlue, owlPurple, owlGreen, owlRed, owlYellow, dark: owlDark },
  fontFamily: "'Inter Variable', Inter, system-ui, -apple-system, 'Segoe UI', sans-serif",
  fontFamilyMonospace: "'JetBrains Mono Variable', 'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, monospace",
  headings: { fontFamily: "'Inter Variable', Inter, system-ui, sans-serif", fontWeight: '600' },
  defaultRadius: 'sm',
  radius: { xs: '2px', sm: '4px', md: '6px', lg: '8px', xl: '10px' },
  fontSizes: { xs: '11px', sm: '12px', md: '13px', lg: '15px', xl: '18px' },
  spacing: { xs: '6px', sm: '8px', md: '12px', lg: '16px', xl: '24px' },
  cursorType: 'pointer',
  components: {
    Button: { defaultProps: { size: 'xs' } },
    ActionIcon: { defaultProps: { size: 'sm', variant: 'subtle', color: 'gray' } },
    TextInput: { defaultProps: { size: 'xs' } },
    PasswordInput: { defaultProps: { size: 'xs' } },
    NumberInput: { defaultProps: { size: 'xs' } },
    Select: { defaultProps: { size: 'xs' } },
    Textarea: { defaultProps: { size: 'xs' } },
    SegmentedControl: { defaultProps: { size: 'xs' } },
    Badge: { defaultProps: { size: 'sm', radius: 'sm', variant: 'light' } },
    Table: { defaultProps: { fz: 'sm', verticalSpacing: 6, horizontalSpacing: 'sm', highlightOnHover: true } },
    Card: { defaultProps: { withBorder: true, padding: 'md', radius: 'md' } },
    Tooltip: { defaultProps: { openDelay: 300, fz: 'xs' } },
    NavLink: { defaultProps: { py: 5 } },
  },
})

/** Exact Night Owl / Light Owl surfaces as CSS variables (body, borders, dimmed text). */
export const cssVariablesResolver: CSSVariablesResolver = () => ({
  variables: { '--sm-mono': 'var(--mantine-font-family-monospace)' },
  light: {
    '--mantine-color-body': lightOwl.bg,
    '--mantine-color-text': lightOwl.fg,
    '--mantine-color-default-border': lightOwl.border,
    '--sm-surface': '#ffffff',
    '--sm-on-filled': '#ffffff',
    '--sm-accent-purple': lightOwl.purple,
    '--sm-accent-teal': lightOwl.teal,
  },
  dark: {
    '--mantine-color-body': nightOwl.bg,
    '--mantine-color-text': nightOwl.fg,
    '--mantine-color-default-border': nightOwl.border,
    '--mantine-color-dimmed': '#8b9fae',
    '--sm-surface': nightOwl.bgRaised,
    '--sm-on-filled': nightOwl.bg,
    '--sm-accent-purple': nightOwl.purple,
    '--sm-accent-teal': nightOwl.teal,
  },
})
