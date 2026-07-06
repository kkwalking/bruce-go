package planning

import (
	"strings"
	"testing"
)

func TestPlanModePromptAsksForConciseFinalResponse(t *testing.T) {
	prompt := Prompt("")
	for _, want := range []string{
		"最终回复只输出一句简短状态",
		"计划创建完成，请审阅",
		"计划已更新，请审阅",
		"不要重复、摘要、改写或节选 markdown 计划正文",
		"完整计划会由系统根据 plan event 单独展示",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "展示当前计划的关键内容") {
		t.Fatalf("prompt should not ask model to summarize plan:\n%s", prompt)
	}
}
