package packs

import (
	"strings"
	"testing"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

func TestLoadDevPack(t *testing.T) {
	r, err := LoadDir("../../packs/dev")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range r.All() {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "deploy,impl,plan" {
		t.Fatalf("types = %s", got)
	}
	impl, _ := r.Get("impl")
	wf := impl.Workflows["feature"]
	if wf.Name != "feature" || len(wf.Stages) != 7 {
		t.Fatalf("feature workflow = %v", wf)
	}
	if wf.Stages[4].Kind != v1.StageKind_STAGE_KIND_PARK || wf.Stages[4].ParkOn != "pr.review" {
		t.Fatalf("awaiting review stage = %v", wf.Stages[4])
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"unknown stage": `
name: x
image: i
stages: [a, b]
default_workflow: w
workflows:
  w: {stages: [{stage: c, kind: STAGE_KIND_AGENT, prompt: p}]}`,
		"out of order": `
name: x
image: i
stages: [a, b]
default_workflow: w
workflows:
  w: {stages: [{stage: b, kind: STAGE_KIND_AGENT, prompt: p}, {stage: a, kind: STAGE_KIND_AGENT, prompt: p}]}`,
		"park without key": `
name: x
image: i
stages: [a]
default_workflow: w
workflows:
  w: {stages: [{stage: a, kind: STAGE_KIND_PARK}]}`,
		"missing default": `
name: x
image: i
stages: [a]
default_workflow: nope
workflows:
  w: {stages: [{stage: a, kind: STAGE_KIND_AGENT, prompt: p}]}`,
	}
	for name, manifest := range cases {
		if _, err := Parse([]byte(manifest)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
