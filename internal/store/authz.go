package store

import (
	"context"
	"fmt"
	"slices"
)

// Authorization (decision D1, 7 Oct 2026). An issue held under a live
// claim may be changed only by its holder's principal or by an admin:
// update, close, reopen, finish, handoff and acceptance changes by anyone
// else are refused with a *ForbiddenError, before anything is written.
// Comments, labels and dependencies stay open to everyone. An admin's
// change to an issue another principal holds is recorded as an
// admin.override event, naming the holder, ahead of the change's own
// events. Admins are a list of principals in starfixd's settings
// (decision D2), passed in Options.Admins; nothing over the protocol
// reads or changes it.

// OpAdminOverride records an admin changing an issue another principal
// holds. Its after state names the holder, the claim's epoch and the
// operation.
const OpAdminOverride Op = "admin.override"

// ReservedPrincipals are the names the server uses itself: the claim
// reaper and the bd importer record their changes under theirs, and the
// health probe of `starfixd upgrade` connects as the last, recording
// nothing. No key may authenticate as one, and none may be an admin, so
// the audit trail cannot be forged.
var ReservedPrincipals = []string{ReaperActor.Principal, "import", "starfixd-upgrade"}

// Reserved reports whether p is a reserved principal name.
func Reserved(p string) bool { return slices.Contains(ReservedPrincipals, p) }

// checkAdmins refuses, with ErrInvalid, an admin in Options.Admins that is
// not a principal name or is reserved.
func checkAdmins(admins []string) error {
	for _, a := range admins {
		if !PrincipalPattern.MatchString(a) || Reserved(a) {
			return fmt.Errorf("%w: admin %q is not a principal name, or is reserved", ErrInvalid, a)
		}
	}
	return nil
}

// IsAdmin reports whether principal is one of the store's admins.
func (s *Store) IsAdmin(principal string) bool { return slices.Contains(s.opts.Admins, principal) }

// guard refuses w's actor a change (op, for the record) to the issue
// whose claim is c when another principal holds it, unless the actor is
// an admin; an admin's change records an admin.override event first.
func (w *wtx) guard(ctx context.Context, c claimRow, op string) error {
	if !c.active(w.now) || c.Holder.Principal == w.actor.Principal {
		return nil
	}
	if !w.admin {
		return &ForbiddenError{ID: c.Issue, Holder: c.Holder, Until: c.ExpiresAt}
	}
	return w.event(ctx, OpAdminOverride, string(c.Issue), nil,
		map[string]any{"holder": c.Holder, "epoch": c.Epoch, "op": op})
}

// ended tells the holder of c, if another session than w's actor holds
// it, that its claim ended, and why. A lapsed claim is told too: the
// reaper would have, and this write ends the claim before it can.
func (w *wtx) ended(ctx context.Context, c claimRow, why string) error {
	if c.Holder.Principal == "" || c.Holder == w.actor {
		return nil
	}
	lapsed := ""
	if !c.active(w.now) {
		lapsed = "lease expired; "
	}
	return w.notify(ctx, InboxItem{To: c.Holder.Principal, Session: c.Holder.Session, Kind: InboxClaimLost, Issue: c.Issue,
		Body: fmt.Sprintf("%s%s by %s/%s (epoch %d): stop work on it", lapsed, why, w.actor.Principal, w.actor.Session, c.Epoch)})
}
