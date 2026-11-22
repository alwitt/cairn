package artifact

import (
	"fmt"
	"mime"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
)

// ======================================================================================
// Plain Text Rules
//
// The I/O free rules the text tools apply to an artifact's content (see DESIGN §7.6): what
// counts as plain text, how line endings are handled, and how an edit is matched. Kept apart
// from the manager so each rule can be exercised directly.

// textApplicationMIMETypes the media types outside `text/*` that carry plain text, beyond the
// `+json` and `+xml` structured syntax suffixes, which are accepted wholesale.
//
// Most of these are what the MIME sniffer reports for common text formats (JSON, NDJSON,
// SubRip); the rest are registered text types a future sniffer may report. CSS, Markdown,
// YAML, and source code without a shebang all sniff as `text/plain` already.
var textApplicationMIMETypes = map[string]bool{
	"application/json":       true,
	"application/x-ndjson":   true,
	"application/xml":        true,
	"application/javascript": true,
	"application/ecmascript": true,
	"application/x-subrip":   true,
	"application/x-sh":       true,
	"application/yaml":       true,
	"application/x-yaml":     true,
	"application/toml":       true,
	"application/sql":        true,
}

/*
isTextMIMEType whether an artifact's MIME type names a plain text format.

This is the cheap first gate, checked against the entry before any content is fetched. It is
not the authority: a few `+xml` types name ZIP containers, and the sniffer reports content
holding ESC or form feed as `text/plain`. `checkPlainText` over the content itself is what
decides (see DESIGN §7.6.1).

	@param mimeType string - the MIME type, with or without parameters
	@returns whether the type names a plain text format
*/
func isTextMIMEType(mimeType string) bool {
	// A sniffed type carries parameters, e.g. `text/plain; charset=utf-8`. A malformed parameter
	// list still leaves a usable base type, so fall back to what precedes the first ';'.
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	}

	switch {
	case strings.HasPrefix(mediaType, "text/"):
		return true
	case strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
		return true
	default:
		return textApplicationMIMETypes[mediaType]
	}
}

/*
checkPlainText verify content is plain text the text tools can work on, and report its line
ending.

Plain text is valid UTF-8 holding no control character other than tab, LF, and CR as part of
CRLF, with one line ending style throughout (see DESIGN §7.6.1). Valid UTF-8 alone is not
enough: NUL and ESC are both valid UTF-8. The control characters are refused because a model
can neither see nor reliably reproduce them, so an edit anchored across one could never match.

	@param content []byte - the content to check
	@returns the content's line ending; LF when the content holds no line break at all
*/
func checkPlainText(content []byte) (models.TextLineEndingENUM, error) {
	if !utf8.Valid(content) {
		return "", goutils.NewBadInputError("content is not valid UTF-8", nil, true)
	}

	lfCount := 0
	crlfCount := 0
	line := 1
	for idx := 0; idx < len(content); {
		char, width := utf8.DecodeRune(content[idx:])
		switch {
		case char == '\n':
			lfCount++
			line++
		case char == '\r':
			if idx+1 >= len(content) || content[idx+1] != '\n' {
				return "", goutils.NewBadInputError(
					fmt.Sprintf("line %d holds a CR that is not part of a CRLF line ending", line),
					nil,
					true,
				)
			}
			crlfCount++
			line++
			// Step over the LF as well, so it is not also counted as an LF line ending.
			width++
		case char == '\t':
		case unicode.IsControl(char):
			return "", goutils.NewBadInputError(
				fmt.Sprintf("line %d holds control character U+%04X", line, char), nil, true,
			)
		}
		idx += width
	}

	if lfCount > 0 && crlfCount > 0 {
		return "", goutils.NewBadInputError(
			fmt.Sprintf(
				"content mixes line endings: %d LF and %d CRLF; it must use one throughout",
				lfCount, crlfCount,
			),
			nil,
			true,
		)
	}

	if crlfCount > 0 {
		return models.TextLineEndingCRLF, nil
	}
	return models.TextLineEndingLF, nil
}

/*
toLF convert CRLF line endings to LF.

The text tools always work in LF, so an agent never has to type a CR to edit a CRLF artifact
(see DESIGN §7.6.2).

	@param text string - the text to convert
	@returns the text with LF line endings
*/
func toLF(text string) string {
	return strings.ReplaceAll(text, "\r\n", "\n")
}

/*
fromLF convert LF text back to a line ending.

	@param text string - text with LF line endings
	@param lineEnding models.TextLineEndingENUM - the line ending to convert to
	@returns the text with the requested line ending
*/
func fromLF(text string, lineEnding models.TextLineEndingENUM) string {
	if lineEnding == models.TextLineEndingCRLF {
		return strings.ReplaceAll(text, "\n", "\r\n")
	}
	return text
}

/*
applyTextEdits apply a list of exact-text replacements to LF content.

The edits apply in order, each to the result of the one before, and all or nothing - on any
failure the content is returned to no one. Matching is an exact substring search with no
regex, fuzzy matching, or whitespace trimming, and an edit that does not match exactly once
is refused rather than guessed at (see DESIGN §7.6.4). Edits are numbered from 1 in errors,
the way an agent counts them.

	@param content string - the content to edit, with LF line endings
	@param edits []models.TextEdit - the edits to apply, in order
	@returns the edited content, with LF line endings
*/
func applyTextEdits(content string, edits []models.TextEdit) (string, error) {
	if len(edits) == 0 {
		return "", goutils.NewValidationError("at least one edit is required", nil, true)
	}

	for idx, edit := range edits {
		position := idx + 1
		// The content is LF, so an edit written against a CRLF view of it must be too.
		oldText := toLF(edit.OldText)
		newText := toLF(edit.NewText)

		if oldText == "" {
			return "", goutils.NewBadInputError(
				fmt.Sprintf("edit %d: old_text is empty", position), nil, true,
			)
		}
		if oldText == newText {
			return "", goutils.NewBadInputError(
				fmt.Sprintf("edit %d: old_text and new_text are the same", position), nil, true,
			)
		}

		matches := strings.Count(content, oldText)
		switch {
		case matches == 0:
			return "", goutils.NewBadInputError(
				fmt.Sprintf(
					"edit %d: old_text not found; it must match the current text exactly, "+
						"including whitespace",
					position,
				),
				nil,
				true,
			)
		case matches > 1 && !edit.ReplaceAll:
			return "", goutils.NewBadInputError(
				fmt.Sprintf(
					"edit %d: old_text found %d times; add more surrounding text or set replace_all",
					position, matches,
				),
				nil,
				true,
			)
		case edit.ReplaceAll:
			content = strings.ReplaceAll(content, oldText, newText)
		default:
			content = strings.Replace(content, oldText, newText, 1)
		}
	}

	return content, nil
}

/*
sliceLines select a range of lines from LF content.

The lines keep their own line breaks, so the slice returned is an exact substring of the
content - which is what lets text copied out of a read be used verbatim as an edit's
`old_text`. An offset past the last line yields an empty slice rather than an error, so a
caller paging through the content simply reaches the end.

	@param content string - the content, with LF line endings
	@param offset int - 1-based number of the first line to return; must be >= 1
	@param limit *int - if set, the most lines to return; must be >= 1
	@returns the selected lines
	@returns the number of lines in the whole content
*/
func sliceLines(content string, offset int, limit *int) (string, int) {
	lines := strings.SplitAfter(content, "\n")
	// A trailing line break, or no content at all, leaves an empty final element that is not
	// a line.
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)

	start := offset - 1
	if start >= total {
		return "", total
	}
	end := total
	// Compared as a remaining count rather than `start + limit`, which a large limit overflows.
	if limit != nil && *limit < end-start {
		end = start + *limit
	}

	return strings.Join(lines[start:end], ""), total
}
