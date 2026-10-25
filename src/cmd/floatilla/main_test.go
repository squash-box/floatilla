package main

import (
	"strings"
	"testing"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
)

func TestMatchTask(t *testing.T) {
	task := func(id, name string, st v1.TaskStatus, key string) *v1.Task {
		t := &v1.Task{Id: id, Name: name, Status: st}
		if key != "" {
			t.Inputs = map[string]string{"tracker.key": key}
		}
		return t
	}
	running, failed := v1.TaskStatus_TASK_STATUS_RUNNING, v1.TaskStatus_TASK_STATUS_FAILED
	tasks := []*v1.Task{
		task("01a1aaa", "float-11-price-every-usage-delta-and", failed, "FLOAT-11"),
		task("01a1bbb", "float-11-price-every-usage-delta-and-r2", running, "FLOAT-11"),
		task("01a1ccc", "float-12-flag-subscription-usage-from", running, "FLOAT-12"),
		task("01a1ddd", "float-13-accumulate-and-display", running, "FLOAT-13"),
		task("01a1eee", "auth-refactor", failed, ""),
		task("01a1fff", "auth-refactor", running, ""),
	}
	for _, c := range []struct{ ref, want, err string }{
		{ref: "float-11-price-every-usage-delta-and", want: "01a1aaa"},
		{ref: "FLOAT-11", want: "01a1bbb"}, // the live retry, not the failed first attempt
		{ref: "float-11", want: "01a1bbb"}, // prefix: the live one
		{ref: "float-12", want: "01a1ccc"},
		{ref: "float-13-acc", want: "01a1ddd"},
		{ref: "01a1c", want: "01a1ccc"},
		{ref: "auth-refactor", want: "01a1fff"},
		{ref: "float-1", err: `"float-1" matches 3 tasks`},
		{ref: "nope", err: `no task "nope"`},
	} {
		got, err := matchTask(tasks, c.ref)
		switch {
		case c.err != "":
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want %q", c.ref, err, c.err)
			}
		case err != nil:
			t.Errorf("%s: %v", c.ref, err)
		case got.Id != c.want:
			t.Errorf("%s: got %s, want %s", c.ref, got.Id, c.want)
		}
	}
}
