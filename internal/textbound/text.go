package textbound

import "unicode/utf8"

// Cut returns at most limit bytes of text without splitting a character.
func Cut(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}
