// dispatch_order.go — the observation that the walk can still account for its
// own ordering. It decides nothing: a gap here is reported, never acted on,
// because a check that refused would take routing down to report a logging gap.
package executor

// checkDispatchOrder warns when a target is about to be dispatched while an
// earlier one was passed over without a recorded reason. A gap does not prove a
// bug; it proves the loop can no longer account for its own ordering.
func (e *TargetExecutor) checkDispatchOrder(walk []walkState, serving int) {
	if e.logger == nil || serving == 0 {
		return
	}
	var unexplained []string
	for i := range serving {
		if !walk[i].explained {
			unexplained = append(unexplained, walk[i].target.ModelID)
		}
	}
	if len(unexplained) == 0 {
		// At Debug, because without a positive trace the only end-to-end
		// assertion available is "it did not warn" — which passes equally
		// against a loop that never calls this. It also answers how far down
		// the list a request got, without a join.
		e.logger.Debug("routing: dispatch order verified",
			"dispatching", walk[serving].target.ModelID, "position", serving, "of", len(walk))
		return
	}
	e.logger.Warn("routing invariant: dispatching past a target that was skipped without a reason",
		"dispatching", walk[serving].target.ModelID,
		"position", serving,
		"unexplained", unexplained,
		"note", "every target before the one that serves must have a recorded attempt or a named skip")
}
