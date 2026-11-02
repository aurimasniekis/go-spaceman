package catalog

import (
	"context"
	"strconv"
	"strings"
)

func formatInt(n int64) string { return strconv.FormatInt(n, 10) }

// DiffLine is one line of a diff.
type DiffLine struct {
	Op   string `json:"op" enum:"same,add,del"`
	Text string `json:"text"`
}

// Diff compares the resolved content of two prompt versions line by line (SM-PROMPT-001).
func (svc *Service) Diff(ctx context.Context, fromID, toID string) ([]DiffLine, error) {
	q := svc.s.R()
	a, err := q.GetPromptVersion(ctx, fromID)
	if err != nil {
		return nil, errVNotFound
	}
	b, err := q.GetPromptVersion(ctx, toID)
	if err != nil {
		return nil, errVNotFound
	}
	ac, err := content(ctx, q, a, 0)
	if err != nil {
		return nil, err
	}
	bc, err := content(ctx, q, b, 0)
	if err != nil {
		return nil, err
	}
	return lineDiff(strings.Split(ac, "\n"), strings.Split(bc, "\n")), nil
}

// lineDiff is a longest-common-subsequence line diff, bounded so huge prompts degrade to replace-all.
func lineDiff(a, b []string) []DiffLine {
	if len(a)*len(b) > 4_000_000 {
		out := make([]DiffLine, 0, len(a)+len(b))
		for _, l := range a {
			out = append(out, DiffLine{Op: "del", Text: l})
		}
		for _, l := range b {
			out = append(out, DiffLine{Op: "add", Text: l})
		}
		return out
	}
	n, m := len(a), len(b)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	out := []DiffLine{}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, DiffLine{Op: "same", Text: a[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{Op: "del", Text: a[i]})
			i++
		default:
			out = append(out, DiffLine{Op: "add", Text: b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{Op: "del", Text: a[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{Op: "add", Text: b[j]})
	}
	return out
}
