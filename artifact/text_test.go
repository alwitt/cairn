package artifact

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/alwitt/cairn/models"
	"github.com/alwitt/goutils"
	"github.com/stretchr/testify/assert"
)

// These exercise the I/O free plain text rules directly. They live in the package itself
// because the rules are unexported, and need nothing the manager's collaborators provide.

// assertBadInput verify an error is a BadInputError, the kind an agent can correct.
func assertBadInput(assert *assert.Assertions, err error, msgAndArgs ...any) {
	assert.NotNil(err, msgAndArgs...)
	var badInput goutils.BadInputError
	assert.True(errors.As(err, &badInput), "expected BadInputError, got %T: %v", err, err)
}

// TestIsTextMIMEType validates the MIME allowlist accepts the text formats the sniffer reports,
// with or without parameters, and nothing binary.
func TestIsTextMIMEType(t *testing.T) {
	assert := assert.New(t)

	accepted := []string{
		"text/plain",
		"text/plain; charset=utf-8",
		"TEXT/PLAIN",
		"text/csv",
		"text/tab-separated-values",
		"text/html; charset=utf-8",
		"text/xml; charset=utf-8",
		"text/javascript",
		"text/x-python",
		"text/x-shellscript",
		"text/calendar",
		"text/rtf",
		"application/json",
		"application/x-ndjson",
		"application/geo+json",
		"application/xml",
		"application/xhtml+xml",
		"application/rss+xml",
		"image/svg+xml",
		"application/javascript",
		"application/x-subrip",
		"application/yaml",
		// a malformed parameter list still carries a usable base type
		"text/plain; charset",
	}
	for _, mimeType := range accepted {
		assert.True(isTextMIMEType(mimeType), "%q should be accepted", mimeType)
	}

	rejected := []string{
		"",
		"application/octet-stream",
		"application/pdf",
		"application/zip",
		"application/x-sqlite3",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"image/png",
		"video/mp4",
	}
	for _, mimeType := range rejected {
		assert.False(isTextMIMEType(mimeType), "%q should be rejected", mimeType)
	}
}

// TestCheckPlainText validates the content check accepts plain text in either line ending and
// refuses every byte a model cannot see or reproduce.
func TestCheckPlainText(t *testing.T) {
	assert := assert.New(t)

	accepted := []struct {
		name    string
		content string
		ending  models.TextLineEndingENUM
	}{
		{name: "empty", content: "", ending: models.TextLineEndingLF},
		{name: "no line break", content: "hello", ending: models.TextLineEndingLF},
		{name: "LF", content: "one\ntwo\n", ending: models.TextLineEndingLF},
		{name: "CRLF", content: "one\r\ntwo\r\n", ending: models.TextLineEndingCRLF},
		{name: "tab", content: "\tindented\n", ending: models.TextLineEndingLF},
		{name: "non-ASCII", content: "café ✓\n", ending: models.TextLineEndingLF},
	}
	for _, testCase := range accepted {
		ending, err := checkPlainText([]byte(testCase.content))
		assert.Nil(err, testCase.name)
		assert.Equal(testCase.ending, ending, testCase.name)
	}

	rejected := []struct {
		name    string
		content string
	}{
		{name: "invalid UTF-8", content: "abc\xff\n"},
		{name: "NUL", content: "a\x00b"},
		{name: "ESC", content: "\x1b[1mbold\x1b[0m\n"},
		{name: "form feed", content: "page\fbreak\n"},
		{name: "DEL", content: "a\x7fb"},
		{name: "C1 control", content: "a\u0085b"},
		{name: "lone CR", content: "a\rb\n"},
		{name: "trailing CR", content: "a\r"},
		{name: "mixed endings", content: "a\nb\r\nc\n"},
	}
	for _, testCase := range rejected {
		_, err := checkPlainText([]byte(testCase.content))
		assertBadInput(assert, err, testCase.name)
	}

	// the error locates the offending character, so an agent can find it
	_, err := checkPlainText([]byte("fine\nnot\x1bfine\n"))
	assertBadInput(assert, err)
	assert.Contains(err.Error(), "line 2")
	assert.Contains(err.Error(), "U+001B")
}

// TestLineEndingConversion validates LF and CRLF round trip.
func TestLineEndingConversion(t *testing.T) {
	assert := assert.New(t)

	assert.Equal("one\ntwo\n", toLF("one\r\ntwo\r\n"))
	assert.Equal("one\ntwo\n", toLF("one\ntwo\n"))
	assert.Equal("one\r\ntwo\r\n", fromLF("one\ntwo\n", models.TextLineEndingCRLF))
	assert.Equal("one\ntwo\n", fromLF("one\ntwo\n", models.TextLineEndingLF))
}

// TestApplyTextEdits validates edits are exact, ordered, refuse to guess, and report which edit
// failed.
func TestApplyTextEdits(t *testing.T) {
	assert := assert.New(t)

	content := "server:\n  port: 8080\n  debug: false\nworker:\n  debug: false\n"

	applied := []struct {
		name     string
		edits    []models.TextEdit
		expected string
	}{
		{
			name:     "single unique match",
			edits:    []models.TextEdit{{OldText: "  port: 8080", NewText: "  port: 9090"}},
			expected: "server:\n  port: 9090\n  debug: false\nworker:\n  debug: false\n",
		},
		{
			name: "replace all",
			edits: []models.TextEdit{
				{OldText: "debug: false", NewText: "debug: true", ReplaceAll: true},
			},
			expected: "server:\n  port: 8080\n  debug: true\nworker:\n  debug: true\n",
		},
		{
			name: "surrounding text makes an ambiguous match unique",
			edits: []models.TextEdit{{
				OldText: "  port: 8080\n  debug: false", NewText: "  port: 8080\n  debug: true",
			}},
			expected: "server:\n  port: 8080\n  debug: true\nworker:\n  debug: false\n",
		},
		{
			name: "later edits apply to the result of earlier ones",
			edits: []models.TextEdit{
				{OldText: "port: 8080", NewText: "port: 9090"},
				{OldText: "port: 9090", NewText: "port: 7070"},
			},
			expected: "server:\n  port: 7070\n  debug: false\nworker:\n  debug: false\n",
		},
		{
			name: "CRLF in an edit matches LF content",
			edits: []models.TextEdit{{
				OldText: "  port: 8080\r\n  debug: false", NewText: "  port: 1\r\n  debug: true",
			}},
			expected: "server:\n  port: 1\n  debug: true\nworker:\n  debug: false\n",
		},
		{
			name:     "empty new text deletes",
			edits:    []models.TextEdit{{OldText: "worker:\n  debug: false\n", NewText: ""}},
			expected: "server:\n  port: 8080\n  debug: false\n",
		},
	}
	for _, testCase := range applied {
		result, err := applyTextEdits(content, testCase.edits)
		assert.Nil(err, testCase.name)
		assert.Equal(testCase.expected, result, testCase.name)
	}

	// no edits at all is a malformed request rather than a failed match
	{
		_, err := applyTextEdits(content, nil)
		var validationErr goutils.ValidationError
		assert.True(errors.As(err, &validationErr), "expected ValidationError, got %T", err)
	}

	refused := []struct {
		name    string
		edits   []models.TextEdit
		message string
	}{
		{
			name:    "not found",
			edits:   []models.TextEdit{{OldText: "port: 1234", NewText: "port: 1"}},
			message: "edit 1: old_text not found",
		},
		{
			name:    "whitespace is significant",
			edits:   []models.TextEdit{{OldText: "port:8080", NewText: "port: 1"}},
			message: "edit 1: old_text not found",
		},
		{
			name:    "ambiguous without replace all",
			edits:   []models.TextEdit{{OldText: "debug: false", NewText: "debug: true"}},
			message: "edit 1: old_text found 2 times; add more surrounding text or set replace_all",
		},
		{
			name:    "replace all still needs a match",
			edits:   []models.TextEdit{{OldText: "absent", NewText: "x", ReplaceAll: true}},
			message: "edit 1: old_text not found",
		},
		{
			name:    "empty old text",
			edits:   []models.TextEdit{{OldText: "", NewText: "x"}},
			message: "edit 1: old_text is empty",
		},
		{
			name:    "no-op edit",
			edits:   []models.TextEdit{{OldText: "server:", NewText: "server:"}},
			message: "edit 1: old_text and new_text are the same",
		},
		{
			name: "a later edit fails on text an earlier one removed",
			edits: []models.TextEdit{
				{OldText: "port: 8080", NewText: "port: 9090"},
				{OldText: "port: 8080", NewText: "port: 7070"},
			},
			message: "edit 2: old_text not found",
		},
	}
	for _, testCase := range refused {
		result, err := applyTextEdits(content, testCase.edits)
		assertBadInput(assert, err, testCase.name)
		assert.Contains(err.Error(), testCase.message, testCase.name)
		// all or nothing: a partial result never escapes
		assert.Empty(result, testCase.name)
	}
}

// TestSliceLines validates line ranges, the end of the content, and that every slice is an
// exact substring of the content.
func TestSliceLines(t *testing.T) {
	assert := assert.New(t)

	limit := func(value int) *int { return &value }

	cases := []struct {
		name     string
		content  string
		offset   int
		limit    *int
		expected string
		total    int
	}{
		{name: "everything", content: "one\ntwo\nthree\n", offset: 1, expected: "one\ntwo\nthree\n", total: 3},
		{name: "one line", content: "one\ntwo\nthree\n", offset: 2, limit: limit(1), expected: "two\n", total: 3},
		{name: "limit past the end", content: "one\ntwo\nthree\n", offset: 3, limit: limit(5), expected: "three\n", total: 3},
		{name: "offset past the end", content: "one\ntwo\nthree\n", offset: 4, expected: "", total: 3},
		{name: "no trailing newline", content: "one\ntwo", offset: 2, expected: "two", total: 2},
		{name: "empty content", content: "", offset: 1, expected: "", total: 0},
		{name: "huge limit does not overflow", content: "one\ntwo\n", offset: 2, limit: limit(math.MaxInt), expected: "two\n", total: 2},
	}
	for _, testCase := range cases {
		slice, total := sliceLines(testCase.content, testCase.offset, testCase.limit)
		assert.Equal(testCase.expected, slice, testCase.name)
		assert.Equal(testCase.total, total, testCase.name)
		assert.True(strings.Contains(testCase.content, slice), "%s: not a substring", testCase.name)
	}
}
