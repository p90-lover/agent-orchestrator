package controllers

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTrimLargeActivityFieldsKeepsHeadTailAndEditSources(t *testing.T) {
	big := "HEAD" + strings.Repeat("x", 200<<10) + "TAIL"
	edit := strings.Repeat("e", 100<<10)
	out := ConversationSnapshotResponse{Activities: []ConversationActivityResponse{{
		Detail: map[string]any{
			"output": big,
			"small":  "kept",
			"text":   big,
			"content": []any{
				map[string]any{"type": "image", "data": big},
				map[string]any{"type": "diff", "oldText": edit, "newText": edit},
			},
			"input": map[string]any{"file_path": "a.go", "content": edit, "old_string": edit},
		},
	}}}
	trimLargeActivityFields(&out)
	detail := out.Activities[0].Detail

	output := detail["output"].(string)
	if len(output) > trimFieldHead+trimFieldTail+64 || !strings.HasPrefix(output, "HEAD") || !strings.HasSuffix(output, "TAIL") ||
		!strings.Contains(output, "bytes omitted") {
		t.Fatalf("output not cut to head and tail: %d bytes", len(output))
	}
	if detail["small"] != "kept" || detail["text"] != big {
		t.Fatal("a small string or prose text was cut")
	}
	content := detail["content"].([]any)
	if data := content[0].(map[string]any)["data"].(string); len(data) > trimFieldHead+trimFieldTail+64 {
		t.Fatalf("image data kept %d bytes", len(data))
	}
	diff := content[1].(map[string]any)
	if diff["oldText"] != edit || diff["newText"] != edit {
		t.Fatal("an edit's diff source was cut")
	}
	input := detail["input"].(map[string]any)
	if input["content"] != edit || input["old_string"] != edit {
		t.Fatal("an edit's input source was cut")
	}
}

func TestTrimMiddleCutsOnRuneBoundaries(t *testing.T) {
	s := strings.Repeat("界", 40<<10) // 3 bytes each: 120 KB
	cut := trimMiddle(s)
	if !utf8.ValidString(cut) {
		t.Fatal("cut split a character")
	}
	if len(cut) >= len(s) || len(cut) > trimFieldHead+trimFieldTail+64 {
		t.Fatalf("cut kept %d of %d bytes", len(cut), len(s))
	}
}
