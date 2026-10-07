package agentsetup

import (
	"errors"
	"strings"
)

// The pointer is a short block in the agent's instruction file that tells
// it to use the starfix tools. It sits between two marker lines, so setup
// can find, replace and remove it without touching the rest of the file.
const (
	beginMarker = "<!-- starfix:begin -->"
	endMarker   = "<!-- starfix:end -->"
)

var pointerBody = []string{
	"## Issue tracking: starfix",
	"Track work with the `starfix` MCP tools; never shell out to `sfx` for it.",
	"Call `prime` when a session starts, unless its output is already in context; then `start` takes an issue and `finish` closes it with a handoff note.",
}

type pointerStyle int

const (
	markdownBlock pointerStyle = iota // a block inside a shared file
	cursorRule                        // a whole file of starfix's own, with Cursor's frontmatter
	jetbrainsRule                     // a whole file of starfix's own, with AI Assistant's frontmatter
)

// frontmatter makes a rule file apply to every request. AI Assistant's
// "apply: always" key is unverified: its rule-file documentation
// describes the rule types in the IDE's settings, not the frontmatter
// keys behind them.
var frontmatter = map[pointerStyle]string{
	cursorRule:    "---\ndescription: Issue tracking with starfix\nalwaysApply: true\n---\n\n",
	jetbrainsRule: "---\napply: always\n---\n\n",
}

func pointerLines(eol string) []string {
	out := make([]string, 0, len(pointerBody)+2)
	for _, l := range append(append([]string{beginMarker}, pointerBody...), endMarker) {
		out = append(out, l+eol)
	}
	return out
}

func ruleContent(style pointerStyle) []byte {
	return []byte(frontmatter[style] + strings.Join(pointerLines(""), "\n") + "\n")
}

// pointerBlock finds the block: the indexes of its begin and end lines.
func pointerBlock(lines []string) (start, end int, ok bool, err error) {
	start = -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case beginMarker:
			if start >= 0 {
				return 0, 0, false, errors.New("a " + beginMarker + " line has no " + endMarker + " line")
			}
			start = i
		case endMarker:
			if start < 0 {
				return 0, 0, false, errors.New("a " + endMarker + " line has no " + beginMarker + " line before it")
			}
			return start, i, true, nil
		}
	}
	if start >= 0 {
		return 0, 0, false, errors.New("a " + beginMarker + " line has no " + endMarker + " line")
	}
	return 0, 0, false, nil
}

// eolOf keeps a CRLF file CRLF.
func eolOf(lines []string) string {
	if len(lines) > 0 && strings.HasSuffix(lines[0], "\r") {
		return "\r"
	}
	return ""
}

func applyPointer(content []byte, style pointerStyle) ([]byte, Result, error) {
	if style != markdownBlock {
		want := ruleContent(style)
		switch {
		case string(content) == string(want):
			return content, Unchanged, nil
		case len(strings.TrimSpace(string(content))) == 0:
			return want, Added, nil
		}
		return want, Updated, nil
	}
	lines := splitLines(content)
	start, end, ok, err := pointerBlock(lines)
	if err != nil {
		return nil, Unchanged, err
	}
	block := pointerLines(eolOf(lines))
	if ok {
		if strings.Join(lines[start:end+1], "\n") == strings.Join(block, "\n") {
			return content, Unchanged, nil
		}
		out := append(append(append([]string{}, lines[:start]...), block...), lines[end+1:]...)
		return joinLines(out), Updated, nil
	}
	if strings.TrimSpace(string(content)) == "" {
		return joinLines(block), Added, nil
	}
	return joinLines(append(append(lines, eolOf(lines)), block...)), Added, nil
}

func removePointer(content []byte, style pointerStyle) ([]byte, Result, error) {
	lines := splitLines(content)
	start, end, ok, err := pointerBlock(lines)
	if err != nil {
		return nil, Unchanged, err
	}
	if !ok {
		return content, Unchanged, nil
	}
	if style != markdownBlock {
		return nil, Removed, nil // the whole file is starfix's
	}
	rest := lines[end+1:]
	// Take the blank line Apply put before a block at the end of the file.
	if len(rest) == 0 && start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	out := append(append([]string{}, lines[:start]...), rest...)
	if strings.TrimSpace(strings.Join(out, "")) == "" {
		return nil, Removed, nil
	}
	return joinLines(out), Removed, nil
}

func pointerRegistered(content []byte, style pointerStyle) bool {
	if style != markdownBlock {
		return string(content) == string(ruleContent(style))
	}
	lines := splitLines(content)
	start, end, ok, err := pointerBlock(lines)
	return err == nil && ok && strings.Join(lines[start:end+1], "\n") == strings.Join(pointerLines(eolOf(lines)), "\n")
}

func pointerSnippet(style pointerStyle) string {
	if style != markdownBlock {
		return string(ruleContent(style))
	}
	return strings.Join(pointerLines(""), "\n") + "\n"
}
