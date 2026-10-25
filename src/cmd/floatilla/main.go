// floatilla is the command-line client for the control plane. The fleet TUI
// will live here too; for now it offers scriptable subcommands.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"

	v1 "github.com/squash-box/floatilla/src/gen/go/floatilla/v1"
	"github.com/squash-box/floatilla/src/gen/go/floatilla/v1/floatillav1connect"
	"github.com/squash-box/floatilla/src/internal/env"
)

const usage = `usage: floatilla [command] [flags]

With no command, floatilla opens the fleet manager TUI.

commands:
  tui      fleet manager TUI     floatilla tui [-demo] [-theme dark|light]
  submit   submit a task         floatilla submit -type impl -name auth-refactor -title "..." [-workflow w] [-input k=v ...]
  ls       list tasks            floatilla ls [-a]
  get      show one task         floatilla get <id|name>
  watch    stream changes        floatilla watch [id|name]   (with a task: also its log)
  answer   answer a prompt       floatilla answer <id|name> -yes | -no [-text "..."]
  stop     stop a task           floatilla stop <id|name> [-reason "..."]
  pause    pause a task          floatilla pause <id|name>
  resume   resume a paused task  floatilla resume <id|name>
  event    post external event   floatilla event <id|name> <key> [-payload '{"..."}']
  types    list process types

env: FLOATILLA_URL (default http://localhost:7420)
`

type cli struct {
	ctx    context.Context
	client floatillav1connect.FleetServiceClient
}

func main() {
	if len(os.Args) < 2 {
		os.Args = append(os.Args, "tui")
	}
	if a := os.Args[1]; a == "help" || a == "-h" || a == "--help" {
		fmt.Print(usage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := &cli{ctx: ctx, client: floatillav1connect.NewFleetServiceClient(http.DefaultClient, env.String("FLOATILLA_URL", "http://localhost:7420"))}

	cmds := map[string]func([]string) error{
		"submit": c.submit, "ls": c.ls, "get": c.get, "watch": c.watch, "answer": c.answer,
		"stop": c.stop, "pause": c.pause, "resume": c.resume, "event": c.event, "types": c.types,
		"tui": c.tui,
	}
	fn, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := fn(os.Args[2:]); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "floatilla:", err)
		os.Exit(1)
	}
}

type kvFlag map[string]string

func (k kvFlag) String() string { return fmt.Sprint(map[string]string(k)) }
func (k kvFlag) Set(s string) error {
	key, val, ok := strings.Cut(s, "=")
	if !ok {
		return fmt.Errorf("want key=value, got %q", s)
	}
	k[key] = val
	return nil
}

// parse handles "<cmd> [positional...] [-flags]" in either order.
func parse(fs *flag.FlagSet, args []string, positional int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if len(pos) < positional {
		return nil, fmt.Errorf("%s: expected %d argument(s)", fs.Name(), positional)
	}
	return pos, nil
}

func (c *cli) submit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	req := &v1.SubmitTaskRequest{Inputs: map[string]string{}}
	fs.StringVar(&req.ProcessType, "type", "", "process type (plan, impl, deploy)")
	fs.StringVar(&req.Name, "name", "", "kebab-case task name")
	fs.StringVar(&req.Title, "title", "", "one-line task title")
	fs.StringVar(&req.Workflow, "workflow", "", "workflow (default: the type's default)")
	fs.StringVar(&req.Instructions, "instructions", "", "detailed instructions (or @file)")
	fs.StringVar(&req.ParentId, "parent", "", "parent task id")
	fs.Var(kvFlag(req.Inputs), "input", "input key=value (repeatable)")
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}
	if path, ok := strings.CutPrefix(req.Instructions, "@"); ok {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		req.Instructions = string(b)
	}
	res, err := c.client.SubmitTask(c.ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	fmt.Println(res.Msg.Task.Id)
	return nil
}

func (c *cli) ls(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	all := fs.Bool("a", false, "include finished tasks")
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}
	res, err := c.client.ListTasks(c.ctx, connect.NewRequest(&v1.ListTasksRequest{IncludeFinished: *all}))
	if err != nil {
		return err
	}
	stages, err := c.stageNames()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tTYPE\tSTATUS\tSTAGE\tCOST\tACTIVITY")
	for _, t := range res.Msg.Tasks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s %s\t%s\t$%.2f\t%s\n",
			t.Id, t.Name, t.ProcessType, glyph(t.Status), label(t.Status),
			stage(t, stages), t.GetUsage().GetCostUsd(), ellipsis(t.Activity, 60))
	}
	return tw.Flush()
}

func (c *cli) get(args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	stages, err := c.stageNames()
	if err != nil {
		return err
	}
	printTask(t, stages)
	return nil
}

func printTask(t *v1.Task, stages map[string][]string) {
	fmt.Printf("%s  [%s] %s  %s %s  stage %s\n", t.Id, strings.ToUpper(t.ProcessType), t.Name, glyph(t.Status), label(t.Status), stage(t, stages))
	fmt.Printf("  %s\n", t.Title)
	fmt.Printf("  workflow %s · run %d · %d tok · $%.2f\n", t.Workflow, t.RunSeq,
		t.GetUsage().GetInputTokens()+t.GetUsage().GetOutputTokens(), t.GetUsage().GetCostUsd())
	if t.Activity != "" {
		fmt.Printf("  activity: %s\n", t.Activity)
	}
	for _, l := range t.Tail {
		fmt.Printf("  │ %s\n", l)
	}
	if t.Prompt != nil {
		fmt.Printf("  ◆ %s  (prompt %s)\n", t.Prompt.Text, t.Prompt.Id)
		if t.Prompt.Detail != "" {
			fmt.Printf("    %s\n", ellipsis(t.Prompt.Detail, 200))
		}
	}
	for _, s := range t.Stages {
		mark := "⠿"
		if s.FinishedAt != nil {
			mark = "✓"
		}
		fmt.Printf("  %s %s", mark, s.Name)
		if s.Summary != "" {
			fmt.Printf(" — %s", ellipsis(oneLine(s.Summary), 100))
		}
		fmt.Println()
	}
	for _, a := range t.Artefacts {
		fmt.Printf("  artefact %-6s %s  %s\n", a.Kind, a.Summary, a.Uri)
	}
	if t.Error != "" {
		fmt.Printf("  error: %s\n", t.Error)
	}
}

func (c *cli) watch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	pos, err := parse(fs, args, 0)
	if err != nil {
		return err
	}
	req := &v1.WatchRequest{}
	if len(pos) > 0 {
		t, err := c.resolve(pos[0])
		if err != nil {
			return err
		}
		req.TaskId = t.Id
	}
	stages, err := c.stageNames()
	if err != nil {
		return err
	}
	stream, err := c.client.Watch(c.ctx, connect.NewRequest(req))
	if err != nil {
		return err
	}
	last := map[string]string{}
	for stream.Receive() {
		switch b := stream.Msg().Body.(type) {
		case *v1.WatchResponse_Task:
			t := b.Task
			line := fmt.Sprintf("%s %-20s %-6s %-8s %-28s %s", glyph(t.Status), t.Name, t.ProcessType, label(t.Status), stage(t, stages), ellipsis(t.Activity, 70))
			if last[t.Id] != line { // heartbeats change nothing visible
				last[t.Id] = line
				fmt.Printf("%s  %s\n", time.Now().Format("15:04:05"), line)
			}
		case *v1.WatchResponse_Log:
			l := b.Log.GetLog()
			at := b.Log.At.AsTime().Local().Format("15:04:05")
			if l.Kind == "" {
				fmt.Printf("%s            │ %s\n", strings.Repeat(" ", 8), l.Text)
			} else {
				fmt.Printf("%s  %-6s %s\n", at, l.Kind, l.Text)
			}
		}
	}
	return stream.Err()
}

func (c *cli) answer(args []string) error {
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "approve")
	no := fs.Bool("no", false, "deny")
	text := fs.String("text", "", "answer text (questions) or note")
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	if *yes == *no && *text == "" {
		return errors.New("answer: pass -yes, -no or -text")
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	_, err = c.client.AnswerPrompt(c.ctx, connect.NewRequest(&v1.AnswerPromptRequest{
		TaskId: t.Id,
		Answer: &v1.PromptAnswer{PromptId: t.GetPrompt().GetId(), Approved: *yes || (*text != "" && !*no), Text: *text},
	}))
	return err
}

func (c *cli) stop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	reason := fs.String("reason", "", "reason")
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	_, err = c.client.StopTask(c.ctx, connect.NewRequest(&v1.StopTaskRequest{TaskId: t.Id, Reason: *reason}))
	return err
}

func (c *cli) pause(args []string) error {
	pos, err := parse(flag.NewFlagSet("pause", flag.ContinueOnError), args, 1)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	_, err = c.client.PauseTask(c.ctx, connect.NewRequest(&v1.PauseTaskRequest{TaskId: t.Id}))
	return err
}

func (c *cli) resume(args []string) error {
	pos, err := parse(flag.NewFlagSet("resume", flag.ContinueOnError), args, 1)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	_, err = c.client.ResumeTask(c.ctx, connect.NewRequest(&v1.ResumeTaskRequest{TaskId: t.Id}))
	return err
}

func (c *cli) event(args []string) error {
	fs := flag.NewFlagSet("event", flag.ContinueOnError)
	payload := fs.String("payload", "", "JSON payload")
	pos, err := parse(fs, args, 2)
	if err != nil {
		return err
	}
	t, err := c.resolve(pos[0])
	if err != nil {
		return err
	}
	_, err = c.client.PostEvent(c.ctx, connect.NewRequest(&v1.PostEventRequest{
		TaskId: t.Id,
		Event:  &v1.ExternalEvent{Key: pos[1], PayloadJson: *payload},
	}))
	return err
}

func (c *cli) types([]string) error {
	res, err := c.client.ListProcessTypes(c.ctx, connect.NewRequest(&v1.ListProcessTypesRequest{}))
	if err != nil {
		return err
	}
	for _, s := range res.Msg.ProcessTypes {
		var wfs []string
		for name := range s.Workflows {
			if name == s.DefaultWorkflow {
				name += "*"
			}
			wfs = append(wfs, name)
		}
		sort.Strings(wfs)
		fmt.Printf("%-8s %-16s %s\n         workflows: %s\n", s.Name, s.Title, strings.Join(s.Stages, " › "), strings.Join(wfs, ", "))
	}
	return nil
}

// resolve finds a task by id, or by name among active tasks.
func (c *cli) resolve(ref string) (*v1.Task, error) {
	res, err := c.client.GetTask(c.ctx, connect.NewRequest(&v1.GetTaskRequest{Id: ref}))
	if err == nil {
		return res.Msg.Task, nil
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		return nil, err
	}
	list, err := c.client.ListTasks(c.ctx, connect.NewRequest(&v1.ListTasksRequest{IncludeFinished: true}))
	if err != nil {
		return nil, err
	}
	return matchTask(list.Msg.Tasks, ref)
}

// matchTask finds the task ref names: an exact name, a tracker key (e.g.
// FLOAT-11), or a unique prefix of a name or id. Unfinished tasks win over
// finished ones, and among equals the latest wins (tasks are in creation
// order).
func matchTask(tasks []*v1.Task, ref string) (*v1.Task, error) {
	exact := func(t *v1.Task) bool {
		return t.Name == ref || (t.Inputs["tracker.key"] != "" && strings.EqualFold(t.Inputs["tracker.key"], ref))
	}
	prefix := func(t *v1.Task) bool {
		return strings.HasPrefix(t.Name, ref) || strings.HasPrefix(t.Id, ref)
	}
	for i, rule := range []func(*v1.Task) bool{exact, prefix} {
		var live, done []*v1.Task
		for _, t := range tasks {
			if !rule(t) {
				continue
			}
			if t.Status == v1.TaskStatus_TASK_STATUS_SUCCEEDED || t.Status == v1.TaskStatus_TASK_STATUS_FAILED {
				done = append(done, t)
			} else {
				live = append(live, t)
			}
		}
		matches := live
		if len(matches) == 0 {
			matches = done
		}
		switch {
		case len(matches) == 0:
			continue
		case len(matches) == 1 || i == 0: // several exact matches: the latest
			return matches[len(matches)-1], nil
		default:
			var names []string
			for _, t := range matches {
				names = append(names, t.Name)
			}
			return nil, fmt.Errorf("%q matches %d tasks: %s", ref, len(matches), strings.Join(names, ", "))
		}
	}
	return nil, fmt.Errorf("no task %q (see floatilla ls)", ref)
}

func (c *cli) stageNames() (map[string][]string, error) {
	res, err := c.client.ListProcessTypes(c.ctx, connect.NewRequest(&v1.ListProcessTypesRequest{}))
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, s := range res.Msg.ProcessTypes {
		out[s.Name] = s.Stages
	}
	return out, nil
}

func stage(t *v1.Task, stages map[string][]string) string {
	names := stages[t.ProcessType]
	if int(t.StageIndex) >= len(names) {
		return fmt.Sprintf("%d", t.StageIndex+1)
	}
	return fmt.Sprintf("%d/%d %s", t.StageIndex+1, len(names), names[t.StageIndex])
}

// Status glyphs and labels follow the fleet design spec (SPEC.md §2.2).
func glyph(s v1.TaskStatus) string {
	switch s {
	case v1.TaskStatus_TASK_STATUS_RUNNING:
		return "⠹"
	case v1.TaskStatus_TASK_STATUS_WAITING:
		return "◆"
	case v1.TaskStatus_TASK_STATUS_FAILED:
		return "✗"
	case v1.TaskStatus_TASK_STATUS_SUCCEEDED:
		return "✓"
	case v1.TaskStatus_TASK_STATUS_PAUSED:
		return "‖"
	case v1.TaskStatus_TASK_STATUS_PARKED:
		return "◇"
	default:
		return "○"
	}
}

func label(s v1.TaskStatus) string {
	if s == v1.TaskStatus_TASK_STATUS_SUCCEEDED {
		return "done"
	}
	return strings.ToLower(strings.TrimPrefix(s.String(), "TASK_STATUS_"))
}

func ellipsis(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
