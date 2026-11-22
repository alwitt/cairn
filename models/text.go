package models

// TextLineEndingENUM the line ending a plain text artifact uses throughout
type TextLineEndingENUM string

const (
	// TextLineEndingLF every line ends with LF
	TextLineEndingLF TextLineEndingENUM = "LF"

	// TextLineEndingCRLF every line ends with CR LF
	TextLineEndingCRLF TextLineEndingENUM = "CRLF"
)

// Values return list of valid TextLineEndingENUM values
func (TextLineEndingENUM) Values() []TextLineEndingENUM {
	return []TextLineEndingENUM{
		TextLineEndingLF,
		TextLineEndingCRLF,
	}
}

// TextEdit one exact-text replacement within a plain text artifact.
//
// The edit is anchored on the text itself, never on a line number: `OldText` must be found in
// the content exactly once, unless `ReplaceAll` is set (see DESIGN §7.6.4).
type TextEdit struct {
	// OldText the exact text to replace
	OldText string `json:"old_text" jsonschema:"the exact text to replace, matched character for character against the artifact's current text with LF line endings, whitespace included - no regex, no trimming. Must not be empty, and must be found exactly once unless replace_all is set; add surrounding lines to make it unique. If you read with line_numbers, leave the line number and the tab after it out of this text."`

	// NewText the text to put in its place; empty deletes OldText
	NewText string `json:"new_text" jsonschema:"the text to put in place of old_text; must differ from it. An empty string deletes old_text."`

	// ReplaceAll replace every occurrence of OldText rather than requiring exactly one
	ReplaceAll bool `json:"replace_all,omitempty" jsonschema:"replace every occurrence of old_text instead of requiring exactly one; default false. old_text must still be found at least once."`
}
