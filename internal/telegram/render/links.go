package render

import (
	"net/url"
	"regexp"
	"strings"
)

// maxURL is the longest link URL kept, in bytes; longer links become their text.
const maxURL = 2048

// AttachmentScheme prefixes attachment references in Markdown bodies (SM-ATT-005).
const AttachmentScheme = "spaceman://attachment/"

var (
	attachmentRE = regexp.MustCompile(`^spaceman://attachment/(att_[0-9a-z]{26})$`)
	languageRE   = regexp.MustCompile(`^[A-Za-z0-9_+#.-]{1,32}$`)
	usernameRE   = regexp.MustCompile(`^@[A-Za-z0-9_]{1,64}$`)
)

// AllowedURL returns the URL to link to, or false when the link must be shown as text. Only absolute http and https
// URLs with a host, and mailto URLs, are allowed (plan §4.3); javascript:, data:, tg:, relative and malformed URLs are
// refused. A "www." autolink without a scheme gets http://, as GFM renders it.
func AllowedURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURL || strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(raw), "www.") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" || u.Hostname() == "" || u.User != nil {
			return "", false
		}
	case "mailto":
		if u.Opaque == "" || !strings.Contains(u.Opaque, "@") {
			return "", false
		}
	default:
		return "", false
	}
	return u.String(), true
}

// attachmentID returns the attachment ID of a spaceman://attachment/ reference.
func attachmentID(dest string) (string, bool) {
	m := attachmentRE.FindStringSubmatch(dest)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// codeLanguage returns the language of a fenced block's info string, or "" when it isn't a plain language name.
func codeLanguage(info string) string {
	lang, _, _ := strings.Cut(strings.TrimSpace(info), " ")
	lang, _, _ = strings.Cut(lang, "\t")
	lang = strings.Trim(lang, "{}")
	if !languageRE.MatchString(lang) {
		return ""
	}
	return strings.ToLower(lang)
}
