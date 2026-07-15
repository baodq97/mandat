package orchestrator

import (
	"errors"
	"testing"
)

// wantTable restates RFC-0001's twelve edges independently of the production
// table (orchestrator.go) so any drift between the encoded table and the RFC
// is a test failure rather than a silent copy.
var wantTable = []transition{
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

// allStates and allEvents are the canonical enumerations the exhaustive product
// test iterates. Adding a State or Event constant to orchestrator.go without
// adding it here trips the count assertions in TestEnumsAreComplete.
var allStates = []State{
	StateQueued,
	StateInProgress,
	StateInReview,
	StateNeedsHuman,
	StateDone,
	StateFailed,
}

var allEvents = []Event{
	EventDispatch,
	EventClaimOK,
	EventSetupFailed,
	EventResultOK,
	EventResultNeedsHuman,
	EventResultInvalid,
	EventGateRed,
	EventRemitViolation,
	EventProbeFailed,
	EventHumanRequeue,
	EventHumanAbandon,
	EventHumanRatify,
}

type fromEvent struct {
	from  State
	event Event
}

func lookup(ts []transition) map[fromEvent]State {
	m := make(map[fromEvent]State, len(ts))
	for _, t := range ts {
		m[fromEvent{t.from, t.event}] = t.to
	}
	return m
}

func TestNextListedEdges(t *testing.T) {
	t.Parallel()
	for _, edge := range wantTable {
		name := string(edge.from) + "/" + string(edge.event)
		if edge.from == StateStart {
			name = "start/" + string(edge.event)
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := Next(edge.from, edge.event)
			if err != nil {
				t.Fatalf("Next(%q, %q) returned error %v, want nil", edge.from, edge.event, err)
			}
			if got != edge.to {
				t.Errorf("Next(%q, %q) = %q, want %q", edge.from, edge.event, got, edge.to)
			}
		})
	}
}

func TestNextExhaustiveProduct(t *testing.T) {
	t.Parallel()
	want := lookup(wantTable)
	fromStates := append([]State{StateStart}, allStates...)

	transitions := 0
	for _, from := range fromStates {
		for _, event := range allEvents {
			to, listed := want[fromEvent{from, event}]
			got, err := Next(from, event)
			if listed {
				transitions++
				if err != nil {
					t.Errorf("Next(%q, %q) returned error %v, want transition to %q", from, event, err, to)
				}
				if got != to {
					t.Errorf("Next(%q, %q) = %q, want %q", from, event, got, to)
				}
				continue
			}
			if err == nil {
				t.Errorf("Next(%q, %q) = %q with nil error, want a no-transition error", from, event, got)
			}
			if got != from {
				t.Errorf("Next(%q, %q) changed state to %q on a no-op, want %q unchanged", from, event, got, from)
			}
		}
	}
	if transitions != len(wantTable) {
		t.Errorf("product covered %d transitioning cells, want %d (the RFC edge count)", transitions, len(wantTable))
	}
}

func TestTableMatchesRFC(t *testing.T) {
	t.Parallel()
	if len(table) != len(wantTable) {
		t.Fatalf("table has %d edges, want %d (RFC-0001 transition table)", len(table), len(wantTable))
	}

	got := lookup(table)
	if len(got) != len(table) {
		t.Fatalf("table has a duplicate (from, event) key: %d rows collapsed to %d edges", len(table), len(got))
	}

	want := lookup(wantTable)
	for k, wantTo := range want {
		gotTo, ok := got[k]
		if !ok {
			t.Errorf("table is missing RFC edge (%q, %q) -> %q", k.from, k.event, wantTo)
			continue
		}
		if gotTo != wantTo {
			t.Errorf("table edge (%q, %q) = %q, want %q", k.from, k.event, gotTo, wantTo)
		}
	}
	for k, gotTo := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("table has extra edge not in RFC: (%q, %q) -> %q", k.from, k.event, gotTo)
		}
	}
}

func TestFailureEdgesAreDistinct(t *testing.T) {
	t.Parallel()
	if got, err := Next(StateQueued, EventSetupFailed); err != nil || got != StateNeedsHuman {
		t.Errorf("Next(queued, setup_failed) = %q, err %v; want needs-human, nil (a recoverable hold, not terminal)", got, err)
	}
	if got, err := Next(StateNeedsHuman, EventHumanAbandon); err != nil || got != StateFailed {
		t.Errorf("Next(needs-human, human_abandon) = %q, err %v; want failed, nil", got, err)
	}

	var toFailed []transition
	for _, edge := range table {
		if edge.to == StateFailed {
			toFailed = append(toFailed, edge)
		}
	}
	if len(toFailed) != 1 {
		t.Fatalf("%d edges reach failed, want exactly 1 (only human_abandon)", len(toFailed))
	}
	only := toFailed[0]
	if only.from != StateNeedsHuman || only.event != EventHumanAbandon {
		t.Errorf("failed is reached via (%q, %q), want (needs-human, human_abandon)", only.from, only.event)
	}

	for _, event := range allEvents {
		if got, err := Next(StateInProgress, event); err == nil && got == StateFailed {
			t.Errorf("Next(in-progress, %q) reached failed; no automatic edge may reach the terminal state", event)
		}
	}
}

func TestInProgressFailureEventsRouteToNeedsHuman(t *testing.T) {
	t.Parallel()
	events := []Event{
		EventResultNeedsHuman,
		EventResultInvalid,
		EventGateRed,
		EventRemitViolation,
		EventProbeFailed,
	}
	for _, event := range events {
		t.Run(string(event), func(t *testing.T) {
			t.Parallel()
			got, err := Next(StateInProgress, event)
			if err != nil {
				t.Fatalf("Next(in-progress, %q) returned error %v, want nil", event, err)
			}
			if got != StateNeedsHuman {
				t.Errorf("Next(in-progress, %q) = %q, want needs-human", event, got)
			}
		})
	}
}

func TestEnumsAreComplete(t *testing.T) {
	t.Parallel()
	if len(allStates) != 6 {
		t.Errorf("allStates has %d entries, want 6; update it when a State constant changes", len(allStates))
	}
	if len(allEvents) != 12 {
		t.Errorf("allEvents has %d entries, want 12; update it when an Event constant changes", len(allEvents))
	}

	eventsUsed := make(map[Event]bool)
	statesUsed := make(map[State]bool)
	knownStates := map[State]bool{StateStart: true}
	for _, s := range allStates {
		knownStates[s] = true
	}
	knownEvents := make(map[Event]bool)
	for _, e := range allEvents {
		knownEvents[e] = true
	}

	for _, edge := range table {
		if !knownStates[edge.from] {
			t.Errorf("table edge uses from-state %q absent from allStates/StateStart", edge.from)
		}
		if !knownStates[edge.to] {
			t.Errorf("table edge uses to-state %q absent from allStates", edge.to)
		}
		if !knownEvents[edge.event] {
			t.Errorf("table edge uses event %q absent from allEvents", edge.event)
		}
		eventsUsed[edge.event] = true
		statesUsed[edge.from] = true
		statesUsed[edge.to] = true
	}

	for _, event := range allEvents {
		if !eventsUsed[event] {
			t.Errorf("event %q triggers no edge; a future edit dropped its transition", event)
		}
	}
	for _, state := range allStates {
		if !statesUsed[state] {
			t.Errorf("state %q is neither a source nor a target of any edge", state)
		}
	}
}

func TestNextUnlistedPairReturnsTypedErrorUnchanged(t *testing.T) {
	t.Parallel()
	const from = StateDone
	const event = EventDispatch

	got, err := Next(from, event)
	if got != from {
		t.Errorf("Next(%q, %q) = %q, want the input state %q unchanged", from, event, got, from)
	}
	var nte *NoTransitionError
	if !errors.As(err, &nte) {
		t.Fatalf("Next(%q, %q) error = %v, want a *NoTransitionError", from, event, err)
	}
	if nte.State != from || nte.Event != event {
		t.Errorf("NoTransitionError{State: %q, Event: %q}, want {%q, %q}", nte.State, nte.Event, from, event)
	}
}
