package planning

import (
	"strings"
	"testing"
)

func TestPlanModePromptAsksForConciseFinalResponse(t *testing.T) {
	prompt := Prompt("")
	for _, want := range []string{
		"make the final response a single brief status sentence",
		"The plan is ready for review",
		"The plan has been updated and is ready for review",
		"Do not reproduce, summarize, paraphrase, or excerpt the Markdown plan",
		"The system presents the complete plan separately from the plan event",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "summarize the current plan's key points") {
		t.Fatalf("prompt should not ask model to summarize plan:\n%s", prompt)
	}
}
