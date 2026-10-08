package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
)

// declaredPaths turns --paths values into the paths an issue declares:
// forward slashes, without a leading "./". The server checks the rest. It
// is never nil, so an empty --paths sends [] and clears the set.
func declaredPaths(l []string) []string {
	out := make([]string, 0, len(l))
	for _, p := range l {
		out = append(out, strings.TrimPrefix(filepath.ToSlash(p), "./"))
	}
	return out
}

// printFiles prints an issue's likely files, declared first, and the
// issues others hold whose files overlap them.
func printFiles(w io.Writer, f *proto.Files) {
	if f == nil || len(f.Paths) == 0 && len(f.Overlaps) == 0 {
		return
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("\n")
	if len(f.Paths) > 0 {
		p("likely files:\n")
		for _, fp := range f.Paths {
			if fp.Source == proto.PathDeclared {
				p("  %s (declared)\n", esc(fp.Path))
			} else {
				p("  %s\n", esc(fp.Path))
			}
		}
		if f.More > 0 {
			p("  and %d more\n", f.More)
		}
	}
	for _, o := range f.Overlaps {
		p("overlaps %s, held by %s (%s)\n", esc(o.ID), esc(o.By), esc(o.Session))
	}
}

// printReady prints ready issues, saying which held issues each one's
// files overlap.
func printReady(w io.Writer, issues []proto.Summary) {
	printSummaries(w, issues, func(i int) string {
		if o := issues[i].Overlaps; len(o) > 0 {
			return "\toverlaps " + strings.Join(escAll(o), ", ")
		}
		return ""
	})
}
