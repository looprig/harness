package serve

import "github.com/looprig/harness/pkg/event"

// isPublicEvent applies the event package's authoritative visibility predicate
// with serve's whole-session scope. Every outward event boundary uses this helper.
func isPublicEvent(ev event.Event) bool {
	return event.ShouldDeliver(allEventsFilter(), ev)
}

func validateStatusEvent(value StatusEvent) error {
	if value.Event == nil {
		return nil
	}
	if !isPublicEvent(value.Event) {
		return &NonPublicEventError{Visibility: value.Event.Visibility()}
	}
	return nil
}

func validateSessionStatus(value SessionStatus) error {
	if value.LastTurn != nil {
		if err := validateStatusEvent(*value.LastTurn); err != nil {
			return err
		}
	}
	if value.LastStep != nil {
		return validateStatusEvent(*value.LastStep)
	}
	return nil
}

func validateEventJournalPage(value EventJournalPage) error {
	for _, statusEvent := range value.Events {
		if err := validateStatusEvent(statusEvent); err != nil {
			return err
		}
	}
	return nil
}
