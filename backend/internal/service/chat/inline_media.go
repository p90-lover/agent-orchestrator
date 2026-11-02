package chat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// A tool that returns an image (an agent reading a screenshot) hands it back as
// base64 inside the item's detail, often twice: once as the content block and
// again inside the JSON-encoded output text. One screenshot made a 1 MB item,
// so a monitoring task that reads screenshots in a loop wrote megabytes per
// minute to the database and its archive, and the chat's conversation read went
// over the app's response limit: the newest items, a pending approval among
// them, stopped showing and the chat looked stuck for hours.
//
// The image itself stays where the agent read it (the agent's own context and
// the file on disk); AO keeps a note of what was there.
const (
	// detailShrinkFloor skips the walk for the ordinary small detail.
	detailShrinkFloor = 64 << 10
	// inlineBlobMin is the shortest string treated as an inline binary blob.
	inlineBlobMin = 16 << 10
	// detailStringMax caps any other single string kept in a detail.
	detailStringMax = 256 << 10
)

// shrinkInlineMedia returns detail with inline base64 blobs replaced by a short
// note and any other string over detailStringMax cut. Detail that is small, or
// is not JSON, comes back unchanged.
func shrinkInlineMedia(detail json.RawMessage) json.RawMessage {
	if len(detail) < detailShrinkFloor {
		return detail
	}
	var value any
	if err := json.Unmarshal(detail, &value); err != nil {
		return detail
	}
	shrunk, changed := shrinkValue(value)
	if !changed {
		return detail
	}
	encoded, err := json.Marshal(shrunk)
	if err != nil {
		return detail
	}
	return encoded
}

func shrinkValue(value any) (any, bool) {
	switch v := value.(type) {
	case map[string]any:
		changed := false
		for key, item := range v {
			if next, ok := shrinkValue(item); ok {
				v[key] = next
				changed = true
			}
		}
		return v, changed
	case []any:
		changed := false
		for i, item := range v {
			if next, ok := shrinkValue(item); ok {
				v[i] = next
				changed = true
			}
		}
		return v, changed
	case string:
		return shrinkString(v)
	default:
		return value, false
	}
}

func shrinkString(s string) (any, bool) {
	if len(s) < inlineBlobMin {
		return s, false
	}
	if looksLikeBase64(s) {
		return fmt.Sprintf("[%d KB of inline binary data not kept by AO]", len(s)>>10), true
	}
	// Output text is often itself JSON (an MCP result serialized into a string)
	// carrying the same blob; shrink inside it and keep it a string.
	if trimmed := strings.TrimSpace(s); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var nested any
		if json.Unmarshal([]byte(trimmed), &nested) == nil {
			if shrunk, changed := shrinkValue(nested); changed {
				if encoded, err := json.Marshal(shrunk); err == nil {
					return string(encoded), true
				}
			}
		}
	}
	if len(s) > detailStringMax {
		cut := detailStringMax
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		return s[:cut] + fmt.Sprintf("…[%d KB more not kept by AO]", (len(s)-cut)>>10), true
	}
	return s, false
}

// looksLikeBase64 reports a long run of base64 (standard or URL alphabet), with
// an optional data: URL prefix and no whitespace: text and code never look like it.
func looksLikeBase64(s string) bool {
	if i := strings.Index(s, ";base64,"); strings.HasPrefix(s, "data:") && i > 0 && i < 100 {
		s = s[i+len(";base64,"):]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '+', c == '/', c == '-', c == '_', c == '=':
		default:
			return false
		}
	}
	return true
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
