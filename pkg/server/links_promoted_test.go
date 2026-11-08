package server

import (
	"net/http"
	"testing"
)

func TestAPIRejectOfAPromotedProposalIsRefusedBeforeLogging(t *testing.T) {
	t.Parallel()
	w := newAPIWorld(t)
	w.start()
	id := w.propose("payments", "billing", 0.9)
	decisionLogged(t, w.decide("admin", `{"decision":"accept","link_id":"`+id+`"}`))
	waitFor(t, "the decision to be applied", func() bool { return w.drained(identityPartition) })
	logged := len(w.entries(identityPartition))
	if rec := w.decide("admin", `{"decision":"reject","link_id":"`+id+`"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400 for rejecting a link that is now asserted", rec.Code)
	}
	if got := len(w.entries(identityPartition)); got != logged || !w.joined("payments", "billing") {
		t.Errorf("got %d entries (want %d), joined=%v, want nothing logged and the records still joined", got, logged, w.joined("payments", "billing"))
	}
}
