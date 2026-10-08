package board

import (
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// TailMax is how many pushed events the board keeps for its tail.
const TailMax = 100

// pageStep is how far PageUp and PageDown move the selection.
const pageStep = 10

// Held is an issue under a live claim: the claim, and the issue's title
// and priority when the board read them.
type Held struct {
	Claim    proto.Claim
	Title    string
	Priority int
}

// An Update is what Live sends the board: a *Snapshot, a *Detail, a
// Pushed event or a Status.
type Update interface{ apply(*Model) }

// Snapshot is what the board lists, as one refresh read it: the ready
// issues, the held ones, the blocked ones. ServerNow and LocalNow are the
// server's clock and this machine's when it was read, so lease ages are
// on the server's clock.
type Snapshot struct {
	Ready     []proto.Summary
	Held      []Held
	HeldMore  int
	Blocked   []proto.BlockedIssue
	ServerNow time.Time
	LocalNow  time.Time
}

// Detail is one issue as show read it, or the refusal that came instead.
type Detail struct {
	ID   string
	Show *proto.ShowResult
	Err  string
}

// Pushed is an event the server pushed.
type Pushed struct{ Event proto.Event }

// Connection states, for Status.
const (
	StateConnecting   = "connecting"
	StateLive         = "live"
	StateReconnecting = "reconnecting"
)

// Status is the connection's state, and what went wrong, if anything:
// a lost connection, or a refusal the board cannot fix by itself.
type Status struct {
	State string
	Note  string
}

// View is what the board shows.
type View int

// The views.
const (
	ViewBoard View = iota
	ViewDetail
	ViewHelp
)

func (v View) String() string {
	return [...]string{"board", "detail", "help"}[v]
}

// Do is what a key asks of the connection.
type Do int

// What keys ask for.
const (
	DoNothing Do = iota
	DoQuit
	DoRefresh
	DoShow // read the issue ID for its detail
	DoHide // the detail closed
)

// Action is a key's request: Do, and for DoShow the issue.
type Action struct {
	Do Do
	ID string
}

// section names the lists a row can be in.
type section int

const (
	secReady section = iota
	secHeld
	secBlocked
)

// row is one selectable issue.
type row struct {
	sec section
	id  string
}

// Model is the board's state: what it lists, the event tail, the
// selection and the view. It is not safe for concurrent use; one loop
// owns it, applying updates and keys and drawing frames.
type Model struct {
	title  string
	snap   Snapshot
	loaded bool
	tail   []proto.Event
	status Status

	rows []row
	sel  int

	view   View
	back   View // the view help returns to
	open   string
	detail *Detail
	scroll int

	// width and height are the terminal's, as Resize last gave them.
	width, height int
}

// Resize tells the model the terminal's size, which bounds scrolling.
func (m *Model) Resize(width, height int) { m.width, m.height = width, height }

// lastScroll is the furthest the detail scrolls: its last line at the
// bottom of the body, as Frame draws it at the size Resize gave.
func (m *Model) lastScroll() int {
	return max(0, len(m.detailLines(m.width, time.Time{}))-(m.height-2))
}

// New returns an empty board titled title, such as the project's name,
// connecting.
func New(title string) *Model {
	return &Model{title: title, status: Status{State: StateConnecting}}
}

// Apply applies an update from Live.
func (m *Model) Apply(u Update) { u.apply(m) }

func (s *Snapshot) apply(m *Model) {
	was := m.selectedRow()
	m.snap, m.loaded = *s, true
	m.rows = m.rows[:0]
	for _, x := range s.Ready {
		m.rows = append(m.rows, row{secReady, x.ID})
	}
	for _, x := range s.Held {
		m.rows = append(m.rows, row{secHeld, x.Claim.ID})
	}
	for _, x := range s.Blocked {
		m.rows = append(m.rows, row{secBlocked, x.ID})
	}
	for i, r := range m.rows {
		if r == was {
			m.sel = i
			return
		}
	}
	m.sel = max(0, min(m.sel, len(m.rows)-1))
}

func (d *Detail) apply(m *Model) {
	if m.view == ViewDetail && d.ID == m.open {
		m.detail = d
	}
}

func (p Pushed) apply(m *Model) {
	m.tail = append(m.tail, p.Event)
	if over := len(m.tail) - TailMax; over > 0 {
		m.tail = append(m.tail[:0], m.tail[over:]...)
	}
}

func (s Status) apply(m *Model) { m.status = s }

// Relevant reports whether an event with op may change what the board
// lists, so it should read the lists again. Comments, labels, acceptance
// items and an admin's override note do not; any other op, including one
// this board does not know, may.
func Relevant(op string) bool {
	switch op {
	case "comment.add", "label.add", "label.remove", "acceptance.tick", "acceptance.untick", "acceptance.waive", "admin.override":
		return false
	}
	return true
}

// Tail returns the pushed events kept, oldest first.
func (m *Model) Tail() []proto.Event { return m.tail }

// Selected returns the selected issue, or "" when nothing is listed.
func (m *Model) Selected() string { return m.selectedRow().id }

func (m *Model) selectedRow() row {
	if m.sel < len(m.rows) {
		return m.rows[m.sel]
	}
	return row{}
}

// View returns the view shown.
func (m *Model) View() View { return m.view }

// Detail returns the open issue's detail, or nil while it is read.
func (m *Model) Detail() *Detail { return m.detail }

// Key applies one key and returns what it asks of the connection.
func (m *Model) Key(k Key) Action {
	switch k {
	case KeyQuit:
		return Action{Do: DoQuit}
	case KeyRefresh:
		return Action{Do: DoRefresh}
	case KeyHelp:
		if m.view == ViewHelp {
			m.view = m.back
		} else {
			m.back, m.view = m.view, ViewHelp
		}
		return Action{}
	}
	switch m.view {
	case ViewHelp:
		if k == KeyBack {
			m.view = m.back
		}
	case ViewDetail:
		return m.detailKey(k)
	default:
		return m.boardKey(k)
	}
	return Action{}
}

func (m *Model) boardKey(k Key) Action {
	last := len(m.rows) - 1
	switch k {
	case KeyUp:
		m.sel = max(0, m.sel-1)
	case KeyDown:
		m.sel = max(0, min(last, m.sel+1))
	case KeyPageUp:
		m.sel = max(0, m.sel-pageStep)
	case KeyPageDown:
		m.sel = max(0, min(last, m.sel+pageStep))
	case KeyHome:
		m.sel = 0
	case KeyEnd:
		m.sel = max(0, last)
	case KeyEnter:
		id := m.Selected()
		if id == "" {
			return Action{}
		}
		m.view, m.open, m.detail, m.scroll = ViewDetail, id, nil, 0
		return Action{Do: DoShow, ID: id}
	}
	return Action{}
}

func (m *Model) detailKey(k Key) Action {
	switch k {
	case KeyUp:
		m.scroll = max(0, m.scroll-1)
	case KeyDown:
		m.scroll = min(m.scroll+1, m.lastScroll())
	case KeyPageUp:
		m.scroll = max(0, m.scroll-pageStep)
	case KeyPageDown:
		m.scroll = min(m.scroll+pageStep, m.lastScroll())
	case KeyHome:
		m.scroll = 0
	case KeyBack:
		m.view, m.open, m.detail = ViewBoard, "", nil
		return Action{Do: DoHide}
	}
	return Action{}
}
