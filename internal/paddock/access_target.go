package paddock

func (s Service) blockAccessTarget(t Task, failure *AccessError) error {
	return s.Store.updateAccessState(t.ID, func(state *accessTaskState) error {
		state.TargetBlock = &accessTargetBlock{Target: accessTargetFor(t), Error: *failure}
		for _, r := range state.Requests {
			if r.Winner == "" && r.View.Target == accessTargetFor(t) {
				r.View.Status = "unknown"
				r.View.SupportedDecisions = []string{}
				r.View.Error = failure
			}
		}
		return nil
	})
}

func accessTargetFailure(state accessTaskState, t Task) *AccessError {
	if state.TargetBlock != nil && state.TargetBlock.Target == accessTargetFor(t) {
		failure := state.TargetBlock.Error
		return &failure
	}
	return nil
}
