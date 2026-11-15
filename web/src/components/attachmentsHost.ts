import { createContext, useContext } from 'react'
import type { AttachmentResolver } from '@/lib/markdown'

/**
 * How a page other than the web app loads the attachments a Markdown body references (the Telegram Mini App: blob images
 * behind a visibility gate, signed downloads). Absent in the web app, which uses the content URLs and its cookie.
 */
export interface AttachmentsHost {
  /** The resolver the renderer uses for a space (stable per space, so a body renders once). */
  resolver(spaceId: string): AttachmentResolver
  /** Wires a rendered body's images and links; returns the cleanup. */
  watch(root: HTMLElement): () => void
}

const Ctx = createContext<AttachmentsHost | null>(null)
export const AttachmentsHostProvider = Ctx.Provider
export const useAttachmentsHost = () => useContext(Ctx)
