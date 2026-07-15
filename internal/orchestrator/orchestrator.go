// Package orchestrator is the pipeline's state authority: the pure-function
// state machine fixed by RFC-0001. Next is deterministic and spends zero tokens
// and zero I/O, so every plane keys off one total function and the journal can
// replay every transition from (state, event) alone.
package orchestrator

import "fmt"

// State is a pipeline state. The zero value StateStart is the pre-creation
// pseudo-state a task occupies before any TaskContract exists; it is not one of
// the six operational states and only the dispatch event leaves it, which is
// why the journal records an empty from_state for dispatch (RFC-0001, AC-05).
type State string

const (
	StateStart      State = ""
	StateQueued     State = "queued"
	StateInProgress State = "in-progress"
	StateInReview   State = "in-review"
	StateNeedsHuman State = "needs-human"
	StateDone       State = "done"
	StateFailed     State = "failed"
)

// Event is a transition trigger. Its string value is the name RFC-0001 gives
// the event and the name the journal records verbatim in its act column.
type Event string

const (
	EventDispatch         Event = "dispatch"
	EventClaimOK          Event = "claim_ok"
	EventSetupFailed      Event = "setup_failed"
	EventResultOK         Event = "result_ok"
	EventResultNeedsHuman Event = "result_needs_human"
	EventResultInvalid    Event = "result_invalid"
	EventGateRed          Event = "gate_red"
	EventRemitViolation   Event = "remit_violation"
	EventProbeFailed      Event = "probe_failed"
	EventHumanRequeue     Event = "human_requeue"
	EventHumanAbandon     Event = "human_abandon"
	EventHumanRatify      Event = "human_ratify"
)

type transition struct {
	from  State
	event Event
	to    State
}

// table is RFC-0001's transition table encoded as data, in RFC row order. Any
// (state, event) pair absent from it is an intentional no-op, not a gap.
var table = []transition{
	{StateStart, EventDispatch, StateQueued},
	{StateQueued, EventClaimOK, StateInProgress},
	{StateQueued, EventSetupFailed, StateNeedsHuman},
	{StateInProgress, EventResultOK, StateInReview},
	{StateInProgress, EventResultNeedsHuman, StateNeedsHuman},
	{StateInProgress, EventResultInvalid, StateNeedsHuman},
	{StateInProgress, EventGateRed, StateNeedsHuman},
	{StateInProgress, EventRemitViolation, StateNeedsHuman},
	{StateInProgress, EventProbeFailed, StateNeedsHuman},
	{StateNeedsHuman, EventHumanRequeue, StateQueued},
	{StateNeedsHuman, EventHumanAbandon, StateFailed},
	{StateInReview, EventHumanRatify, StateDone},
}

// NoTransitionError reports that no edge leaves State on Event. Next returns it
// with the input state unchanged so an unlisted pair can never silently advance.
type NoTransitionError struct {
	State State
	Event Event
}

func (e *NoTransitionError) Error() string {
	return fmt.Sprintf("orchestrator: no transition from state %q on event %q", e.State, e.Event)
}

// Next returns the state reached from state on event. It is total over the
// enumerated inputs: an unlisted pair returns the input state and a
// *NoTransitionError, never a panic and never a silent transition.
func Next(state State, event Event) (State, error) {
	for _, t := range table {
		if t.from == state && t.event == event {
			return t.to, nil
		}
	}
	return state, &NoTransitionError{State: state, Event: event}
}
