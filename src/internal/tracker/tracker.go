// Package tracker connects floatilla to a story tracker. Approved plans are
// published as work items; items an operator moves to "Ready for agent"
// become implementation tasks; task status flows back to the items; and the
// tracker's review states drive the impl workflow's review loop.
package tracker

import (
	"context"
	"strings"
	"time"
)

// State is a tracker state in floatilla's terms. Each adapter maps it to a
// state of its own.
type State string

const (
	StateBacklog   State = "backlog"
	StateReady     State = "ready"     // an operator hands the item to an agent
	StateWorking   State = "working"   // an agent is on it
	StateNeedsYou  State = "needs-you" // the agent is blocked on a person
	StateInReview  State = "in-review" // a PR is open
	StateDone      State = "done"      // accepted
	StateCancelled State = "cancelled" // dropped; stops the agent
)

const (
	// Source is the external source floatilla stamps on items it creates.
	Source = "floatilla"
	// Label marks items floatilla created.
	Label = "floatilla"
	// PlanLabel sends an item to a planning agent instead of an
	// implementation agent.
	PlanLabel = "plan"
	// commentPrefix marks floatilla's own comments, so they aren't read back
	// as review feedback.
	commentPrefix = "floatilla · "
)

// Item is a work item. State is empty for tracker states floatilla doesn't use.
type Item struct {
	ID             string
	Key            string
	URL            string
	Title          string
	Description    string
	State          State
	Labels         []string
	ParentID       string
	ExternalSource string
	ExternalID     string
}

func (it Item) HasLabel(name string) bool {
	for _, l := range it.Labels {
		if strings.EqualFold(l, name) {
			return true
		}
	}
	return false
}

type NewItem struct {
	Title              string
	Description        string
	AcceptanceCriteria []string
	Priority           string
	State              State
	ExternalID         string
	// ParentID makes the item a child (sub-item) of another.
	ParentID string
}

// Block is a part of a comment after its first line: a heading with
// preformatted text and/or a list.
type Block struct {
	Heading string
	Pre     string
	List    []string
}

// Tracker is a story tracker adapter.
type Tracker interface {
	Name() string
	// EnsureSetup resolves the project and creates missing states and labels.
	EnsureSetup(ctx context.Context) error
	Items(ctx context.Context) ([]Item, error)
	Create(ctx context.Context, in NewItem) (Item, error)
	SetState(ctx context.Context, itemID string, s State) error
	Comment(ctx context.Context, itemID, text string, blocks ...Block) error
	// Comments returns people's comments created after since.
	Comments(ctx context.Context, itemID string, since time.Time) ([]string, error)
	Link(ctx context.Context, itemID, url, title string) error
	// Group collects one plan's items (a Plane module, a Jira epic) when the
	// plan didn't come from an item they can be children of.
	Group(ctx context.Context, name, description string, itemIDs []string) error
}
