package cmagateway

import (
	"github.com/wu8685/ahsir/internal/cmagateway/cma"
	"github.com/wu8685/ahsir/internal/cmagateway/store"
)

// deliverUserMessage records the inbound user message in the event log and then
// drives one turn. It is the core CMA "send a user.message" op, invoked by the
// sendEvents handler. (In cma-service this lived in busdriver.go alongside the
// eventbus helpers; only this one is part of the CMA facade.)
func (s *Server) deliverUserMessage(rec *store.SessionRecord, content []cma.ContentBlock) {
	um := newEvent(cma.EvtUserMessage)
	um.Content = content
	s.emit(rec, um)
	s.runTurn(rec, textOf(content))
}
