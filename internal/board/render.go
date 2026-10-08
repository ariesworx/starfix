package board

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/safetext"
)

// Layout: below wideMin columns the board is one column; a held issue's
// session is cut to sessionMax runes.
const (
	wideMin    = 60
	gap        = 2
	sessionMax = 10
)

// style is how a span is drawn when color is on.
type style uint8

const (
	plain style = iota
	bold
	dim
	warn
	bad
	good
)

// sgr is each style's Select Graphic Rendition parameter.
var sgr = [...]string{plain: "", bold: "1", dim: "2", warn: "33", bad: "31", good: "32"}

// span is a run of text in one style. Text from the server is escaped
// before it becomes a span.
type span struct {
	text string
	st   style
}

// line is one row of the frame; sel draws it as the selection.
type line struct {
	spans []span
	sel   bool
}

// lineOf is a line of spans, not selected.
func lineOf(parts ...span) line { return line{spans: parts} }

// plainSpan and styled make spans.
func plainSpan(t string) span        { return span{t, plain} }
func styled(t string, st style) span { return span{t, st} }

// esc escapes a single-line field from the server.
func esc(t string) string { return safetext.Line(t) }

// Frame draws the model for a terminal width columns wide and height
// rows high, at now: this machine's clock, in whose location times are
// shown. It returns exactly height lines, none wider than width, cut with
// "…" where they would be. With color it adds SGR codes; without, the
// lines hold no control character, and the selection is the row marked
// ">". Everything from the server is escaped (safetext), so it cannot
// move the cursor or rewrite the screen.
func (m *Model) Frame(width, height int, now time.Time, color bool) []string {
	if height < 1 {
		return nil
	}
	out := make([]string, 0, height)
	out = append(out, m.header(width).render(width, color))
	if height >= 2 {
		bodyH := height - 2
		var body []line
		switch m.view {
		case ViewHelp:
			body = window(helpLines(), 0, bodyH)
		case ViewDetail:
			lines := m.detailLines(width, now)
			start := min(m.scroll, max(0, len(lines)-bodyH))
			body = lines[start:min(len(lines), start+bodyH)]
		default:
			body = m.boardLines(width, bodyH, now, color)
		}
		for _, l := range body {
			out = append(out, l.render(width, color))
		}
		for len(out) < height-1 {
			out = append(out, "")
		}
		out = append(out, m.footer().render(width, color))
	}
	return out
}

// header names the board and says how the connection stands.
func (m *Model) header(width int) line {
	left := styled("starfix "+esc(m.title), bold)
	var right []span
	switch m.status.State {
	case StateLive:
		right = []span{styled("live", good)}
	case StateReconnecting:
		right = []span{styled("reconnecting", bad)}
	default:
		right = []span{styled("connecting…", dim)}
	}
	if m.status.Note != "" {
		right = append(right, plainSpan(": "+esc(m.status.Note)))
	}
	l := lineOf(left)
	pad := width - spansWidth(l.spans) - spansWidth(right)
	l.spans = append(l.spans, plainSpan(strings.Repeat(" ", max(1, pad))))
	l.spans = append(l.spans, right...)
	return l
}

func (m *Model) footer() line {
	switch m.view {
	case ViewHelp:
		return lineOf(styled("? or esc back  q quit", dim))
	case ViewDetail:
		return lineOf(styled("esc back  j/k scroll  r refresh  ? help  q quit", dim))
	}
	return lineOf(styled("j/k move  enter open  r refresh  ? help  q quit", dim))
}

// boardLines lays out the lists and the event tail in bodyH rows: two
// columns from wideMin, one below it.
func (m *Model) boardLines(width, bodyH int, now time.Time, color bool) []line {
	if !m.loaded {
		return window([]line{lineOf(plainSpan("  reading the board…"))}, 0, bodyH)
	}
	serverNow := now.Add(m.snap.ServerNow.Sub(m.snap.LocalNow))
	ready, held, blocked := m.readyLines(), m.heldLines(serverNow), m.blockedLines()
	events := m.eventLines(now)
	if width < wideMin {
		all := join(ready, held, blocked, events)
		return window(all, selLine(all), bodyH)
	}
	left, right := join(ready, blocked), held
	// The tail takes at least a third of a tall enough body, and any rows
	// the lists leave.
	evH := 0
	if bodyH >= 8 {
		evH = min(len(events), max(3, bodyH/3, bodyH-max(len(left), len(right))))
	}
	listH := bodyH - evH
	lw := (width - gap) / 2
	rw := width - gap - lw
	left, right = window(left, selLine(left), listH), window(right, selLine(right), listH)
	var out []line
	for i := range max(len(left), len(right)) {
		var l, r line
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		out = append(out, beside(l, lw, r, rw, color))
	}
	if evH > 0 {
		out = append(out, events[0]) // the heading
		out = append(out, events[len(events)-(evH-1):]...)
	}
	return out
}

// join stacks sections with a blank row between them.
func join(sections ...[]line) []line {
	var out []line
	for i, sec := range sections {
		if i > 0 {
			out = append(out, line{})
		}
		out = append(out, sec...)
	}
	return out
}

// selLine is the index of the selected row in lines, or 0.
func selLine(lines []line) int {
	for i, l := range lines {
		if l.sel {
			return i
		}
	}
	return 0
}

// window returns at most h of lines, from start, or around the selection
// at start when lines is longer than h: the selection stays in view.
func window(lines []line, at, h int) []line {
	if len(lines) <= h {
		return lines
	}
	start := max(0, min(at-h/2, len(lines)-h))
	if at < h {
		start = 0
	}
	return lines[start : start+h]
}

// beside draws l, cut or padded to lw columns, then the gap, then r cut
// to rw. A selected half keeps its marking.
func beside(l line, lw int, r line, rw int, color bool) line {
	ls, rs := l.render(lw, color), r.render(rw, color)
	if rs == "" {
		return line{spans: []span{{ls, rawStyle}}}
	}
	pad := lw - visibleWidth(ls)
	return line{spans: []span{{ls + strings.Repeat(" ", pad+gap) + rs, rawStyle}}}
}

// rawStyle marks a span already rendered, color codes and all.
const rawStyle style = 255

func heading(name string, n, more int) line {
	t := fmt.Sprintf("%s %d", name, n)
	if more > 0 {
		t = fmt.Sprintf("%s %d+%d", name, n, more)
	}
	return lineOf(styled(t, bold))
}

// mark is a row's prefix: ">" on the selected row.
func (m *Model) mark(sec section, id string) (span, bool) {
	if m.selectedRow() == (row{sec, id}) {
		return plainSpan("> "), true
	}
	return plainSpan("  "), false
}

func (m *Model) readyLines() []line {
	out := []line{heading("Ready", len(m.snap.Ready), 0)}
	for _, x := range m.snap.Ready {
		mk, sel := m.mark(secReady, x.ID)
		l := line{spans: []span{mk, plainSpan(esc(x.ID) + fmt.Sprintf(" P%d ", x.Priority))}, sel: sel}
		if len(x.Overlaps) > 0 {
			l.spans = append(l.spans, styled("[overlaps "+ids(x.Overlaps)+"]", warn), plainSpan(" "))
		}
		l.spans = append(l.spans, plainSpan(esc(x.Title)))
		out = append(out, l)
	}
	return noneIfEmpty(out)
}

func (m *Model) heldLines(serverNow time.Time) []line {
	out := []line{heading("Held", len(m.snap.Held), m.snap.HeldMore)}
	for _, h := range m.snap.Held {
		c := h.Claim
		mk, sel := m.mark(secHeld, c.ID)
		l := line{spans: []span{mk, plainSpan(fmt.Sprintf("%s P%d %s/%s ", esc(c.ID), h.Priority, esc(c.By), cut(esc(c.Session), sessionMax))),
			styled(proto.Span(max(0, serverNow.Sub(c.ClaimedAt))), dim), plainSpan(" ")}, sel: sel}
		if !c.ExpiresAt.After(serverNow) {
			l.spans = append(l.spans, styled("[lapsed]", warn), plainSpan(" "))
		}
		l.spans = append(l.spans, plainSpan(esc(h.Title)))
		out = append(out, l)
	}
	return noneIfEmpty(out)
}

func (m *Model) blockedLines() []line {
	out := []line{heading("Blocked", len(m.snap.Blocked), 0)}
	for _, b := range m.snap.Blocked {
		mk, sel := m.mark(secBlocked, b.ID)
		l := line{spans: []span{mk, plainSpan(esc(b.ID) + fmt.Sprintf(" P%d ", b.Priority))}, sel: sel}
		if len(b.BlockedBy) > 0 {
			l.spans = append(l.spans, styled("[by "+ids(b.BlockedBy[:1])+plus(len(b.BlockedBy)-1+b.More)+"]", dim), plainSpan(" "))
		}
		l.spans = append(l.spans, plainSpan(esc(b.Title)))
		out = append(out, l)
	}
	return noneIfEmpty(out)
}

// eventLines are the tail's heading and rows, oldest first.
func (m *Model) eventLines(now time.Time) []line {
	out := []line{lineOf(styled("Events", bold))}
	for _, e := range m.tail {
		out = append(out, lineOf(plainSpan("  "), styled(e.At.In(now.Location()).Format("15:04:05"), dim),
			plainSpan(fmt.Sprintf(" %s/%s %s %s", esc(e.Principal), cut(esc(e.Session), sessionMax), esc(e.Op), esc(e.Issue)))))
	}
	if len(m.tail) == 0 {
		out = append(out, lineOf(styled("  none since the board opened", dim)))
	}
	return out
}

func noneIfEmpty(sec []line) []line {
	if len(sec) == 1 {
		return append(sec, lineOf(styled("  none", dim)))
	}
	return sec
}

// ids lists issue ids, escaped: the first two, then how many more.
func ids(xs []string) string {
	shown := xs[:min(2, len(xs))]
	out := make([]string, len(shown))
	for i, x := range shown {
		out[i] = esc(x)
	}
	return strings.Join(out, ",") + plus(len(xs)-len(shown))
}

func plus(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" +%d", n)
}

// cut shortens t to at most n runes, ending in "…" when cut.
func cut(t string, n int) string {
	if utf8.RuneCountInString(t) <= n {
		return t
	}
	if n <= 0 {
		return ""
	}
	r := []rune(t)
	return string(r[:n-1]) + "…"
}

func (m *Model) detailLines(width int, now time.Time) []line {
	d := m.detail
	switch {
	case d == nil:
		return []line{lineOf(styled("  reading "+esc(m.open)+"…", dim))}
	case d.Show == nil:
		return []line{lineOf(styled("cannot read "+esc(d.ID)+": "+esc(d.Err), bad))}
	}
	sh, is := d.Show, d.Show.Issue
	out := []line{lineOf(styled(esc(is.ID)+"  "+esc(is.Title), bold))}
	facts := fmt.Sprintf("%s, P%d, %s", esc(is.Status), is.Priority, esc(is.Type))
	if is.Assignee != "" {
		facts += ", assigned to " + esc(is.Assignee)
	}
	out = append(out, lineOf(plainSpan(facts)))
	serverNow := now.Add(m.snap.ServerNow.Sub(m.snap.LocalNow))
	if c := sh.Claim; c != nil {
		t := fmt.Sprintf("held by %s/%s on %s, epoch %d", esc(c.By), esc(c.Session), esc(c.Machine), c.Epoch)
		if !c.ClaimedAt.IsZero() {
			t += ", for " + proto.Span(max(0, serverNow.Sub(c.ClaimedAt)))
		}
		out = append(out, lineOf(plainSpan(t)))
	}
	if len(is.Labels) > 0 {
		labels := make([]string, len(is.Labels))
		for i, l := range is.Labels {
			labels[i] = esc(l)
		}
		out = append(out, lineOf(plainSpan("labels: "+strings.Join(labels, ", "))))
	}
	for _, dep := range sh.Deps {
		if dep.From == is.ID {
			out = append(out, lineOf(plainSpan(fmt.Sprintf("depends on %s (%s)", esc(dep.To), esc(dep.Type)))))
		} else {
			out = append(out, lineOf(plainSpan(fmt.Sprintf("%s depends on it (%s)", esc(dep.From), esc(dep.Type)))))
		}
	}
	if f := sh.Files; f != nil {
		for _, o := range f.Overlaps {
			out = append(out, lineOf(styled(fmt.Sprintf("overlaps %s, held by %s/%s", esc(o.ID), esc(o.By), esc(o.Session)), warn)))
		}
		for _, p := range f.Paths {
			out = append(out, lineOf(plainSpan(fmt.Sprintf("file: %s (%s)", esc(p.Path), esc(p.Source)))))
		}
	}
	if len(sh.Items) > 0 {
		out = append(out, lineOf(plainSpan("acceptance:")))
		for _, it := range sh.Items {
			box := map[string]string{"ticked": "[x]", "waived": "[-]"}[it.State]
			if box == "" {
				box = "[ ]"
			}
			out = append(out, lineOf(plainSpan(fmt.Sprintf("  %s %d %s", box, it.N, esc(it.Text)))))
		}
	}
	if h := sh.Handoff; h != nil {
		out = append(out, lineOf(plainSpan("handoff from "+esc(h.Author)+": "+esc(h.Body))))
		if h.Next != "" {
			out = append(out, lineOf(plainSpan("next: "+esc(h.Next))))
		}
	}
	if is.Body != "" {
		out = append(out, line{})
		for _, para := range strings.Split(strings.TrimRight(is.Body, "\n"), "\n") {
			for _, w := range wrap(esc(para), width) {
				out = append(out, lineOf(plainSpan(w)))
			}
		}
	}
	return out
}

// wrap breaks t into lines of at most width runes, at spaces where it
// can.
func wrap(t string, width int) []string {
	if width < 1 {
		return nil
	}
	var out []string
	cur := ""
	for _, word := range strings.Fields(t) {
		for utf8.RuneCountInString(word) > width {
			if cur != "" {
				out, cur = append(out, cur), ""
			}
			r := []rune(word)
			out, word = append(out, string(r[:width])), string(r[width:])
		}
		switch {
		case cur == "":
			cur = word
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(word) <= width:
			cur += " " + word
		default:
			out, cur = append(out, cur), word
		}
	}
	if cur != "" || len(out) == 0 {
		out = append(out, cur)
	}
	return out
}

func helpLines() []line {
	rows := [][2]string{
		{"j, k, arrows", "move"},
		{"PgUp, PgDn, g, G", "a page, the top, the bottom"},
		{"enter, right", "open the issue"},
		{"esc, left", "back"},
		{"r", "read everything again"},
		{"?", "this help"},
		{"q, ctrl-c", "quit"},
	}
	out := []line{lineOf(styled("Keys", bold))}
	for _, r := range rows {
		out = append(out, lineOf(plainSpan(fmt.Sprintf("  %-18s%s", r[0], r[1]))))
	}
	marks := []struct {
		mark, means string
		st          style
	}{
		{"[overlaps ID]", "its files overlap those of ID, which another session holds", warn},
		{"[by ID +N]", "the open issues it waits for", dim},
		{"[lapsed]", "the lease ran out; the reaper frees it soon", warn},
	}
	out = append(out, line{}, lineOf(styled("Marks", bold)))
	for _, mk := range marks {
		out = append(out, lineOf(plainSpan("  "), styled(fmt.Sprintf("%-16s", mk.mark), mk.st), plainSpan(mk.means)))
	}
	out = append(out,
		line{},
		lineOf(plainSpan("The board is read-only. It updates as the server pushes changes.")))
	return out
}

// render draws l in at most width columns.
func (l line) render(width int, color bool) string {
	if width <= 0 {
		return ""
	}
	var b strings.Builder
	left := width
	if spansWidth(l.spans) > width {
		left = width - 1 // room for the "…"
	}
	cutAt := false
	for _, sp := range l.spans {
		if sp.st == rawStyle {
			b.WriteString(sp.text) // already fitted
			continue
		}
		t := sp.text
		if n := utf8.RuneCountInString(t); n > left {
			t, cutAt = string([]rune(t)[:max(0, left)]), true
		}
		left -= utf8.RuneCountInString(t)
		writeStyled(&b, t, sp.st, l.sel, color)
		if cutAt {
			break
		}
	}
	if cutAt || left < 0 {
		writeStyled(&b, "…", plain, l.sel, color)
	}
	return b.String()
}

func writeStyled(b *strings.Builder, t string, st style, sel, color bool) {
	if t == "" {
		return
	}
	var codes []string
	if color && sel {
		codes = append(codes, "7")
	}
	if color && st != plain {
		codes = append(codes, sgr[st])
	}
	if len(codes) == 0 {
		b.WriteString(t)
		return
	}
	fmt.Fprintf(b, "\x1b[%sm%s\x1b[0m", strings.Join(codes, ";"), t)
}

func spansWidth(spans []span) int {
	n := 0
	for _, sp := range spans {
		if sp.st == rawStyle {
			n += visibleWidth(sp.text)
		} else {
			n += utf8.RuneCountInString(sp.text)
		}
	}
	return n
}

// visibleWidth counts the runes of t outside the board's own SGR codes.
func visibleWidth(t string) int {
	n := 0
	for i := 0; i < len(t); {
		if strings.HasPrefix(t[i:], "\x1b[") {
			if j := strings.IndexByte(t[i:], 'm'); j >= 0 {
				i += j + 1
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(t[i:])
		i += size
		n++
	}
	return n
}
