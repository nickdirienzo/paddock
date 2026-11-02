package paddock

import "testing"

func TestPlanRequest(t *testing.T) {
	for _, body := range []string{
		"tests are failing", "Looks good, thanks!", "Why was this changed?",
		"@mozziebot tests are failing", "@mozziebot[bot] tests are failing",
		"paddock: delegate: independent work", "paddock: status",
		"```\npaddock: delegate: quoted code\n```",
	} {
		t.Run(body, func(t *testing.T) {
			got := PlanRequest(Request{Body: body})
			if got.Action != "message" || got.Text != body {
				t.Fatalf("%+v", got)
			}
		})
	}
	if got := PlanRequest(Request{Body: " \n\t"}); got.Action != "ignore" {
		t.Fatal(got)
	}
}
