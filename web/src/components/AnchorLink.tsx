import { Anchor, type AnchorProps } from '@mantine/core'
import { createLink, type LinkComponent } from '@tanstack/react-router'
import { forwardRef, type AnchorHTMLAttributes } from 'react'

const MantineAnchor = forwardRef<HTMLAnchorElement, AnchorProps & AnchorHTMLAttributes<HTMLAnchorElement>>((props, ref) => <Anchor ref={ref} {...props} />)
MantineAnchor.displayName = 'MantineAnchor'

const Created = createLink(MantineAnchor)

/** A Mantine Anchor that is a type-checked router link (path params included). */
export const AnchorLink: LinkComponent<typeof MantineAnchor> = (props) => <Created preload="intent" {...props} />
