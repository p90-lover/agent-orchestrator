package chat

import (
	"encoding/json"
	"strings"
	"testing"
)

// The shape an agent's Read of a screenshot produced (seen live): the image as a
// content block and again inside the JSON-encoded output, 1 MB for one item.
func TestShrinkInlineMediaDropsScreenshotsBothPlaces(t *testing.T) {
	image := strings.Repeat("/9j/4AAQSkZJRgABAQAAAQABAAD", 20000) // ~540 KB of base64
	output, _ := json.Marshal([]any{map[string]any{"source": map[string]any{"data": image, "media_type": "image/jpeg"}, "type": "image"}})
	detail, _ := json.Marshal(map[string]any{
		"tool":    "Read",
		"input":   map[string]any{"file_path": `aiTemp\monitor\w7411.jpg`},
		"content": []any{map[string]any{"type": "image", "content": map[string]any{"data": image, "mimeType": "image/jpeg"}}},
		"output":  string(output),
	})
	shrunk := shrinkInlineMedia(detail)
	if len(shrunk) > 4096 {
		t.Fatalf("shrunk detail = %d bytes, want a small item", len(shrunk))
	}
	var got map[string]any
	if err := json.Unmarshal(shrunk, &got); err != nil {
		t.Fatalf("shrunk detail is not JSON: %v", err)
	}
	if got["tool"] != "Read" || got["input"].(map[string]any)["file_path"] != `aiTemp\monitor\w7411.jpg` {
		t.Fatalf("ordinary fields changed: %v", got)
	}
	data := got["content"].([]any)[0].(map[string]any)["content"].(map[string]any)["data"].(string)
	if !strings.Contains(data, "inline binary data not kept") {
		t.Fatalf("content data = %q", data)
	}
	out, ok := got["output"].(string)
	if !ok || !strings.Contains(out, "inline binary data not kept") || !strings.Contains(out, "image/jpeg") {
		t.Fatalf("output = %q, want the JSON text kept with its image dropped", out)
	}
}

func TestShrinkInlineMediaKeepsOrdinaryDetail(t *testing.T) {
	small := json.RawMessage(`{"command":"git status","output":"clean"}`)
	if got := shrinkInlineMedia(small); string(got) != string(small) {
		t.Fatalf("small detail changed: %s", got)
	}
	// Large, but text: a long log line is not mistaken for base64 and stays whole.
	text := strings.Repeat("line of build output with spaces\n", 3000) // ~100 KB
	large, _ := json.Marshal(map[string]any{"output": text})
	if got := shrinkInlineMedia(large); string(got) != string(large) {
		t.Fatal("text under the string cap changed")
	}
	// Text over the cap is cut on a character boundary with a note.
	huge, _ := json.Marshal(map[string]any{"output": strings.Repeat("é and text ", 40000)}) // ~480 KB
	var got map[string]string
	if err := json.Unmarshal(shrinkInlineMedia(huge), &got); err != nil {
		t.Fatal(err)
	}
	if len(got["output"]) > detailStringMax+64 || !strings.Contains(got["output"], "more not kept by AO") {
		t.Fatalf("huge text = %d bytes", len(got["output"]))
	}
	if notJSON := json.RawMessage(strings.Repeat("x", detailShrinkFloor+1)); string(shrinkInlineMedia(notJSON)) != string(notJSON) {
		t.Fatal("non-JSON detail changed")
	}
}
