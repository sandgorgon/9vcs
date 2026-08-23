package patches

import (
	"crypto/rand"
	"encoding/hex"
)

// Line is one line in a file's line graph: a stable identity plus its
// current text. The identity survives edits to *other* lines, which is what
// lets independent patches touching different lines commute.
type Line struct {
	ID      string
	Content string
}

// newLineID generates a fresh, globally-unique line identity. It is
// intentionally independent of content or position: duplicate lines (e.g.
// two blank lines) must still get distinct, stable identities.
func newLineID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failing is not a recoverable condition
	}
	return hex.EncodeToString(b[:])
}

// Diff computes the ops that transform old (the last-recorded line graph
// for a file) into new (its current raw text content, split into lines),
// along with the resulting line graph after those ops are applied.
//
// This is a plain LCS alignment on line content, not Pijul's full
// categorical patch construction — a deliberately simplified starting point
// (see PLAN.md "Open items to revisit").
func Diff(old []Line, new []string) (ops []LineOp, newIndex []Line) {
	oldContent := make([]string, len(old))
	for i, l := range old {
		oldContent[i] = l.Content
	}

	common := lcs(oldContent, new)

	var (
		oi, ni, ci int
		prevID     string // id of the most recently emitted line in the new sequence
	)
	for ci < len(common) {
		// Delete old lines that don't survive to the next common line.
		for oi < len(old) && old[oi].Content != common[ci] {
			ops = append(ops, LineOp{Kind: OpDelete, ID: old[oi].ID})
			oi++
		}
		// Insert new lines that appear before the next common line.
		for ni < len(new) && new[ni] != common[ci] {
			id := newLineID()
			ops = append(ops, LineOp{Kind: OpInsert, ID: id, After: prevID, Content: new[ni]})
			newIndex = append(newIndex, Line{ID: id, Content: new[ni]})
			prevID = id
			ni++
		}
		// The common line itself: retained unchanged, keep its old id.
		newIndex = append(newIndex, Line{ID: old[oi].ID, Content: old[oi].Content})
		prevID = old[oi].ID
		oi++
		ni++
		ci++
	}
	for oi < len(old) {
		ops = append(ops, LineOp{Kind: OpDelete, ID: old[oi].ID})
		oi++
	}
	for ni < len(new) {
		id := newLineID()
		ops = append(ops, LineOp{Kind: OpInsert, ID: id, After: prevID, Content: new[ni]})
		newIndex = append(newIndex, Line{ID: id, Content: new[ni]})
		prevID = id
		ni++
	}
	return ops, newIndex
}

// lcs returns the longest common subsequence of a and b, by content. O(n*m)
// dynamic program — fine for the file sizes this scaffold targets; not
// intended as the final word on diff performance.
func lcs(a, b []string) []string {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			i++
		default:
			j++
		}
	}
	return out
}
