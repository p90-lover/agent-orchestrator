package controllers

import (
	"fmt"
	"unicode/utf8"
)

// A live poll (Coding Tools reads a working chat's newest items every second)
// asks for trimLargeFields. A screenshot's base64 data or a long tool result is
// hundreds of KB that every poll encoded and the client parsed, though the
// client shows only a tool's first 2 KB of output or a terminal's last 16 KB.
// Measured on 2026-10-22 over ten recent chats: 8.8 MB of newest-100 pages, 2.9
// MB trimmed. A string over trimFieldOver keeps its first trimFieldHead and last
// trimFieldTail bytes. Edit sources and prose stay whole: clients count an
// edit's changed lines from them and show a finished reasoning in full.
const (
	trimFieldOver = 64 << 10
	trimFieldHead = 2 << 10
	trimFieldTail = 16 << 10
	trimMaxDepth  = 12
)

var untrimmedFields = map[string]bool{
	"oldText": true, "newText": true, "old_string": true, "new_string": true,
	"patch": true, "diff": true, "content": true, "text": true,
}

// trimLargeActivityFields cuts long strings in every activity's detail in place.
// Each detail map is decoded fresh for this response, so nothing shared changes.
func trimLargeActivityFields(out *ConversationSnapshotResponse) {
	for i := range out.Activities {
		if out.Activities[i].Detail != nil {
			trimLargeValue(out.Activities[i].Detail, "", 0)
		}
	}
}

func trimLargeValue(value any, key string, depth int) any {
	switch v := value.(type) {
	case string:
		if len(v) <= trimFieldOver || untrimmedFields[key] {
			return v
		}
		return trimMiddle(v)
	case map[string]any:
		if depth < trimMaxDepth {
			for field, item := range v {
				v[field] = trimLargeValue(item, field, depth+1)
			}
		}
		return v
	case []any:
		// Items inherit their list's name, so a list of edit sources stays whole too.
		if depth < trimMaxDepth {
			for i, item := range v {
				v[i] = trimLargeValue(item, key, depth+1)
			}
		}
		return v
	default:
		return v
	}
}

// trimMiddle keeps the first trimFieldHead and last trimFieldTail bytes of s,
// cut on rune boundaries, with a note of how much was left out.
func trimMiddle(s string) string {
	head := trimFieldHead
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tail := len(s) - trimFieldTail
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + fmt.Sprintf("\n… [%d bytes omitted] …\n", tail-head) + s[tail:]
}
