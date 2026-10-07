package mcpserver

import (
	"errors"
	"strings"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/proto"
)

// explain rephrases an error for an agent (design §12 item 5). The server's
// code and message are kept; its fix, written for a person at a shell
// (`sfx show …`), becomes the agent's next step in terms of tools.
// Fixes only a person can carry out (keys, config, upgrades) are handed to
// the user.
func explain(err error) toolErr {
	var lost *lostError
	var dial *dialError
	var pe *proto.Error
	errors.As(err, &pe)
	e := toolErr{Code: string(proto.CodeUnavailable), Message: err.Error()}
	if pe != nil {
		e.Code, e.Message = string(pe.Code), pe.Message
	}
	switch {
	case errors.As(err, &lost) && lost.retried:
		e.Fix = "starfix reconnected and the retry failed too; try again later, and if it persists tell the user: " + personFix(pe)
	case errors.As(err, &lost):
		e.Fix = "the connection dropped and the next call reconnects; this write may have applied, so check with show before repeating it"
	case errors.As(err, &dial):
		if pe == nil || pe.Fix == "" {
			e.Fix = "tell the user starfix cannot connect: check " + client.ConfigFile + " and the network"
		} else {
			e.Fix = "tell the user starfix cannot connect: " + pe.Fix
		}
	case pe == nil:
		e.Fix = "retry once; if it fails again, tell the user"
	default:
		e.Fix = nextStep(pe)
	}
	return e
}

// nextStep is the agent's next step after a server refusal.
func nextStep(pe *proto.Error) string {
	switch pe.Code {
	case proto.CodeNotFound:
		return "find the id with list or ready"
	case proto.CodeConflict:
		if strings.HasPrefix(pe.Fix, "re-read") {
			return "call show for the current rev, then retry with that rev if your change still applies"
		}
		return "retry"
	case proto.CodeExists:
		return "omit id to have one generated, or call show on the existing issue"
	case proto.CodeCycle:
		return "remove an edge with dep (action rm), or choose another parent"
	case proto.CodeInvalid:
		switch {
		case strings.HasPrefix(pe.Fix, "reopen"):
			return "call reopen first"
		case pe.Fix == "nothing to do":
			return "nothing to do"
		case strings.HasPrefix(pe.Fix, "upgrade"):
			return "tell the user: " + pe.Fix
		}
		return "correct the arguments and retry"
	case proto.CodeUnavailable:
		return "retry once; if it fails again, tell the user: " + personFix(pe)
	}
	return "tell the user: " + personFix(pe)
}

func personFix(pe *proto.Error) string {
	if pe == nil || pe.Fix == "" {
		return "starfix cannot reach its server"
	}
	return pe.Fix
}
