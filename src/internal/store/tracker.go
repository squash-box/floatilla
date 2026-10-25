package store

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/squash-box/floatilla/src/internal/tracker"
)

var _ tracker.Links = (*Store)(nil)

func (s *Store) TrackerLinks(ctx context.Context) ([]tracker.Link, error) {
	rows, err := s.pool.Query(ctx,
		`select item_id, item_key, url, task_id, plan_task_id, story_key, synced_state, since, accepted, pr_linked, dispatches, settled
		 from tracker_links order by item_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (tracker.Link, error) {
		var l tracker.Link
		var state string
		err := r.Scan(&l.ItemID, &l.ItemKey, &l.URL, &l.TaskID, &l.PlanTaskID, &l.StoryKey, &state, &l.Since, &l.Accepted, &l.PRLinked, &l.Dispatches, &l.Settled)
		l.SyncedState = tracker.State(state)
		return l, err
	})
}

func (s *Store) SaveTrackerLink(ctx context.Context, l tracker.Link) error {
	_, err := s.pool.Exec(ctx,
		`insert into tracker_links (item_id, item_key, url, task_id, plan_task_id, story_key, synced_state, since, accepted, pr_linked, dispatches, settled)
		 values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		 on conflict (item_id) do update set item_key = $2, url = $3, task_id = $4, plan_task_id = $5, story_key = $6,
		   synced_state = $7, since = $8, accepted = $9, pr_linked = $10, dispatches = $11, settled = $12`,
		l.ItemID, l.ItemKey, l.URL, l.TaskID, l.PlanTaskID, l.StoryKey, string(l.SyncedState), l.Since, l.Accepted, l.PRLinked, l.Dispatches, l.Settled)
	return err
}

func (s *Store) PublishedPlans(ctx context.Context) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `select plan_task_id from tracker_plans`)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

func (s *Store) MarkPlanPublished(ctx context.Context, planTaskID string) error {
	_, err := s.pool.Exec(ctx, `insert into tracker_plans (plan_task_id) values ($1) on conflict do nothing`, planTaskID)
	return err
}
