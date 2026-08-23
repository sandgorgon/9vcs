package patches

// Apply replays ops against old, reconstructing the line graph they were
// computed against. For ops produced by Diff(old, new), Apply(old, ops)
// reproduces exactly the newIndex Diff itself returned — this is what lets
// history be replayed from stored ops alone, without keeping a snapshot of
// every intermediate state.
func Apply(old []Line, ops []LineOp) []Line {
	cur := append([]Line(nil), old...)
	for _, op := range ops {
		switch op.Kind {
		case OpDelete:
			cur = deleteLine(cur, op.ID)
		case OpInsert:
			cur = insertLine(cur, op.After, Line{ID: op.ID, Content: op.Content})
		}
	}
	return cur
}

func deleteLine(lines []Line, id string) []Line {
	for i, l := range lines {
		if l.ID == id {
			return append(lines[:i], lines[i+1:]...)
		}
	}
	return lines
}

func insertLine(lines []Line, after string, l Line) []Line {
	if after == "" {
		return append([]Line{l}, lines...)
	}
	for i, x := range lines {
		if x.ID == after {
			out := make([]Line, 0, len(lines)+1)
			out = append(out, lines[:i+1]...)
			out = append(out, l)
			out = append(out, lines[i+1:]...)
			return out
		}
	}
	return append(lines, l)
}
