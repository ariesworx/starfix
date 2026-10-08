package mcpserver

import (
	"errors"
	"strconv"
	"strings"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/safetext"
)

// explain rephrases an error for an agent (design §12 item 5). The server's
// code and message are kept; its fix, written for a person at a shell
// (`sfx show …`), becomes the agent's next step in terms of tools.
// Fixes only a person can carry out (keys, config, upgrades) are handed to
// the user, quoted: the server chose that text, so it is relayed as data
// for the user, never as the agent's instruction (C-4). The message is
// the server's too, escaped onto one line so it cannot add a fix line.
func explain(err error) toolErr {
	var lost *lostError
	var dial *dialError
	var pe *proto.Error
	errors.As(err, &pe)
	e := toolErr{Code: string(proto.CodeUnavailable), Message: err.Error()}
	if pe != nil {
		e.Code, e.Message = string(pe.Code), pe.Message
	}
	e.Message = safetext.Line(e.Message)
	switch {
	case errors.As(err, &lost) && lost.retried:
		e.Fix = "starfix reconnected and the retry failed too; try again later, and if it persists tell the user" + personFix(pe)
	case errors.As(err, &lost):
		e.Fix = "the connection dropped and the next call reconnects; this write may have applied, so check with show before repeating it"
	case errors.As(err, &dial):
		if pe == nil || pe.Fix == "" {
			e.Fix = "tell the user starfix cannot connect: check " + client.ConfigFile + " and the network"
		} else {
			e.Fix = "tell the user starfix cannot connect" + personFix(pe)
		}
	case pe == nil:
		e.Fix = "retry once; if it fails again, tell the user"
	default:
		e.Fix = nextStep(pe)
	}
	return e
}

// nextStep is the agent's next step after a server refusal. It tells
// refusals apart by their code and the proto.Fix phrase their fix opens
// with (FixNothing is a whole fix, FixEditText an ending). internal/server
// writes those fixes with the same phrases, and its TestFixPhrases fails
// if a fix loses its phrase.
func nextStep(pe *proto.Error) string {
	switch pe.Code {
	case proto.CodeNotFound:
		if strings.HasPrefix(pe.Fix, proto.FixSeeBlocked) {
			return "nothing is ready: call blocked to see why, or create an issue"
		}
		return "find the id with list or ready"
	case proto.CodeConflict:
		switch {
		case strings.HasPrefix(pe.Fix, proto.FixTakeNext):
			return "call start with the next ready id named above"
		case strings.HasPrefix(pe.Fix, proto.FixTakeOver):
			return "another session of yours holds it: call start with take: true only if the user says that session has stopped; otherwise pick other work"
		case strings.HasPrefix(pe.Fix, proto.FixLeaveIt):
			return "someone else holds this issue: pick other work with start, or tell the user if it must move"
		case strings.HasPrefix(pe.Fix, proto.FixReread):
			return "call show for the current rev, then retry with that rev if your change still applies"
		}
		return "retry"
	case proto.CodeExists:
		return "omit id to have one generated, or call show on the existing issue"
	case proto.CodeCycle:
		return "remove an edge with dep (action rm), or choose another parent"
	case proto.CodeInvalid:
		switch {
		case strings.HasPrefix(pe.Fix, proto.FixReopen):
			return "call reopen first"
		case strings.HasPrefix(pe.Fix, proto.FixStart):
			return "call start to take it; update cannot set in_progress"
		case strings.HasPrefix(pe.Fix, proto.FixRelease):
			return "it is claimed: call finish or handoff with release: true first if it is yours; otherwise leave it"
		case pe.Fix == proto.FixNothing:
			return "nothing to do"
		case strings.HasPrefix(pe.Fix, proto.FixUpgrade):
			return "tell the user" + personFix(pe)
		}
		return "correct the arguments and retry"
	case proto.CodeAcceptance:
		if strings.HasSuffix(pe.Fix, proto.FixEditText) {
			return "the edit drops open acceptance items: keep them in the text, or tell the user they must be ticked or waived first"
		}
		return "call finish with ticked: [numbers met] and waived: {number: reason} for the rest; show lists the items"
	case proto.CodeForbidden:
		if strings.HasPrefix(pe.Fix, proto.FixAsk) {
			return "someone else holds this issue: do not change it; leave a comment, pick other work with start, or tell the user"
		}
		return "this is for a starfix admin: tell the user"
	case proto.CodeUnavailable:
		return "retry once; if it fails again, tell the user" + personFix(pe)
	case proto.CodeBusy:
		return "the server is limiting your requests: wait a few seconds, then retry once; if it is refused again, tell the user" + personFix(pe)
	}
	return "tell the user" + personFix(pe)
}

// personFix relays the server's fix for the user, quoted.
func personFix(pe *proto.Error) string {
	if pe == nil || pe.Fix == "" {
		return ": starfix cannot reach its server"
	}
	return ", quoting the server: " + strconv.Quote(pe.Fix)
}
