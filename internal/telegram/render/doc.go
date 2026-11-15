// Package render turns SpaceMan Markdown into Telegram messages and Telegram messages back into Markdown (ADR-0083,
// plan §4.3).
//
// Markdown is parsed with goldmark (CommonMark plus the GFM extensions) into this package's own small AST
// ([Document]). Telegram structures are built from that AST only: Telegram's MarkdownV2 or HTML parser is never fed
// untrusted text, so there is no escaping class of bugs on the way out.
//
//   - [Document.Entities] produces plain text plus MessageEntity values (offsets in UTF-16 code units), split into at
//     most [Options.MaxParts] parts of at most [Options.Limit] units each, on block boundaries where possible.
//   - [Document.Rich] produces an InputRichMessage built from blocks (Bot API 10.1-10.3), or an error when the body
//     exceeds the rich-message limits, in which case the caller sends the entity parts instead.
//   - [Markdown] converts an inbound text with entities into Markdown, escaping the Markdown metacharacters of plain
//     runs.
//
// The feature set follows the web renderer (ADR-0082): GFM tables, task lists, strikethrough and autolinks, footnotes,
// attachment images, fenced code with a language and Mermaid diagrams (shown as code, with a "View diagram" Mini App
// button), and raw HTML reduced to its text. The same hostile-input caps apply before parsing (see [Limits]).
//
// Body content is untrusted data. Links are kept only for http, https and mailto URLs; every other link becomes its
// text.
package render
