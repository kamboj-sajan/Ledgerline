package payments

// Status is a payment's lifecycle state.
type Status string

// Payment states.
const (
	StatusCreated    Status = "created"
	StatusAuthorized Status = "authorized"
	StatusCaptured   Status = "captured"
	StatusRefunded   Status = "refunded"
	StatusFailed     Status = "failed"
)

// Statuses lists every state, in lifecycle order.
var Statuses = []Status{StatusCreated, StatusAuthorized, StatusCaptured, StatusRefunded, StatusFailed}

// Event is something that happens to a payment.
type Event string

// Payment events.
const (
	EventApprove       Event = "approve"        // the acquirer approves the authorization
	EventDecline       Event = "decline"        // the acquirer declines, or fraud rules block
	EventCapture       Event = "capture"        // the merchant captures (all or part of) the authorization
	EventVoid          Event = "void"           // the merchant releases the authorization
	EventExpire        Event = "expire"         // the authorization was not captured in time
	EventPartialRefund Event = "partial_refund" // a refund that leaves part of the capture unrefunded
	EventFullRefund    Event = "full_refund"    // the refund that brings refunds up to the captured amount
)

// Events lists every event.
var Events = []Event{EventApprove, EventDecline, EventCapture, EventVoid, EventExpire, EventPartialRefund, EventFullRefund}

// transitions is the complete state machine. For each event, it lists the
// states the event may happen in and the state it leads to. Anything not
// listed is rejected with ErrInvalidTransition (HTTP 409
// invalid_state_transition).
//
// Keying by event, not just by (from, to), matters because two different
// events can share a (from, to) pair's endpoint: a void (authorized → failed)
// and a decline (created → failed) both end in failed, but voiding a created
// payment is not allowed.
var transitions = map[Event]map[Status]Status{
	EventApprove:       {StatusCreated: StatusAuthorized},
	EventDecline:       {StatusCreated: StatusFailed},
	EventCapture:       {StatusAuthorized: StatusCaptured},
	EventVoid:          {StatusAuthorized: StatusFailed},
	EventExpire:        {StatusAuthorized: StatusFailed},
	EventPartialRefund: {StatusCaptured: StatusCaptured},
	EventFullRefund:    {StatusCaptured: StatusRefunded},
}

// Next returns the state that event e leads to from state from, or false if
// e is not allowed in that state.
func Next(from Status, e Event) (Status, bool) {
	to, ok := transitions[e][from]
	return to, ok
}

// CanTransition reports whether any event moves a payment from one status
// to another.
func CanTransition(from, to Status) bool {
	for _, byFrom := range transitions {
		if t, ok := byFrom[from]; ok && t == to {
			return true
		}
	}
	return false
}

// target returns the state an event leads to wherever it is allowed (each
// event has exactly one target). Used for error messages.
func (e Event) target() Status {
	for _, to := range transitions[e] {
		return to
	}
	return ""
}

// Terminal reports whether no event can move a payment out of s.
func (s Status) Terminal() bool {
	for _, byFrom := range transitions {
		if _, ok := byFrom[s]; ok {
			return false
		}
	}
	return true
}
