package paddock

import "testing"

func TestPlanRequest(t *testing.T) {
	for _, tc := range []struct{ body, action, kind string }{
		{"Looks good, thanks!", "ignore", "discussion"},
		{"I fixed this already.", "ignore", "discussion"},
		{"Why was this changed?", "ignore", "discussion"},
		{"Please fix the nil check.", "message", "instruction"},
		{"Can you add a regression test?", "message", "instruction"},
		{"@team-helper why does this fail?", "message", "question"},
		{"@team-helper-impersonator why does this fail?", "ignore", "discussion"},
		{"paddock: status?", "reply", "question"},
		{"@team-helper: delegate: add separate documentation", "delegate", "separate_work"},
		{"Please create a separate task to test Windows", "delegate", "separate_work"},
		{"paddock: reply: Thanks, investigating.", "reply", "instruction"},
		{"paddock: message: investigate the race", "message", "instruction"},
		{"paddock: delegate:", "ignore", "discussion"},
		{"paddock:", "ignore", "discussion"},
		{"```\npaddock: delegate: run this\n```", "ignore", "discussion"},
	} {
		t.Run(tc.body, func(t *testing.T) {
			got := PlanRequest(Request{Body: tc.body, Address: "team-helper"})
			if got.Action != tc.action || got.Kind != tc.kind {
				t.Fatalf("%+v", got)
			}
		})
	}
}
