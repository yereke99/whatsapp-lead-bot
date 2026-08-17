package campaigns

import (
	"context"
	"fmt"
)

// Diagnostics answers one question: is the queue still telling the truth?
//
// Reconciliation repairs what it finds, which is the right behaviour but a poor
// alarm — a system that silently fixes itself looks identical to one that never
// broke. These checks report the state instead of changing it, so a defect that
// reconciliation cannot repair (a job pointing at a deleted step, a lock nobody
// will ever release) becomes visible rather than merely survivable.
//
// Check 1 is the one that matters most. It is the exact shape of the production
// failure: enabled steps, running enrollments, and no job connecting them. On a
// healthy system it returns zero, always.

// CheckResult is one diagnostic and how many rows failed it.
type CheckResult struct {
	Name    string `json:"name"`
	Detail  string `json:"detail"`
	Count   int    `json:"count"`
	Healthy bool   `json:"healthy"`
}

// ConsistencyReport is the full set, plus a single verdict for the dashboard.
type ConsistencyReport struct {
	Checks  []CheckResult `json:"checks"`
	Healthy bool          `json:"healthy"`
	Issues  int           `json:"issues"`
}

// consistencyChecks is the catalogue. Each query counts offending rows; zero is
// always the healthy answer, which keeps adding a check to one line of SQL.
var consistencyChecks = []struct {
	name   string
	detail string
	query  string
}{
	{
		name:   "steps_without_jobs",
		detail: "enabled steps with no scheduled job for a running enrollment",
		// The invariant, stated as SQL. This is the check that would have caught
		// the original bug the moment it happened.
		query: `
			SELECT count(*)
			FROM campaign_contacts cc
			JOIN campaigns camp ON camp.id = cc.campaign_id
			JOIN campaign_steps cs ON cs.campaign_id = cc.campaign_id
			WHERE cc.status = 'ACTIVE'
			  AND camp.status IN ('ACTIVE', 'PAUSED')
			  AND camp.archived_at IS NULL
			  AND cs.enabled
			  AND NOT EXISTS (
				SELECT 1 FROM scheduled_messages sm
				WHERE sm.enrollment_id = cc.id
				  AND sm.campaign_step_id = cs.id
				  AND sm.run_number = cc.run_number
			  )`,
	},
	{
		name:   "duplicate_jobs",
		detail: "more than one job for the same enrollment, step and run",
		// Should be structurally impossible: the unique constraint enforces it.
		// Checked anyway, because a constraint that was never verified is a
		// belief rather than a guarantee.
		query: `
			SELECT COALESCE(sum(n - 1), 0) FROM (
				SELECT count(*) AS n FROM scheduled_messages
				GROUP BY enrollment_id, campaign_step_id, run_number
				HAVING count(*) > 1
			)`,
	},
	{
		name:   "completed_with_unfinished_steps",
		detail: "enrollments closed while an enabled step is still unresolved",
		query: `
			SELECT count(*)
			FROM campaign_contacts cc
			JOIN campaign_steps cs ON cs.campaign_id = cc.campaign_id
			WHERE cc.status = 'COMPLETED'
			  AND cs.enabled
			  AND NOT EXISTS (
				SELECT 1 FROM scheduled_messages sm
				WHERE sm.enrollment_id = cc.id
				  AND sm.campaign_step_id = cs.id
				  AND sm.run_number = cc.run_number
				  AND sm.status IN ('SENT', 'FAILED', 'CANCELLED')
			  )`,
	},
	{
		name:   "pending_in_closed_campaign",
		detail: "jobs still queued for a campaign that is no longer running",
		query: `
			SELECT count(*)
			FROM scheduled_messages sm
			JOIN campaigns camp ON camp.id = sm.campaign_id
			WHERE sm.status = 'PENDING'
			  AND (camp.status IN ('DRAFT', 'COMPLETED', 'ARCHIVED') OR camp.archived_at IS NOT NULL)`,
	},
	{
		name:   "stuck_processing",
		detail: "jobs held in PROCESSING for more than an hour",
		// The lease normally returns these within minutes. An hour means the
		// recovery sweep itself is not running.
		query: `
			SELECT count(*) FROM scheduled_messages
			WHERE status = 'PROCESSING'
			  AND (locked_at IS NULL OR locked_at < ts_add(now(), -3600))`,
	},
	{
		name:   "failed_without_error",
		detail: "failed jobs carrying no error to explain them",
		query: `
			SELECT count(*) FROM scheduled_messages
			WHERE status = 'FAILED' AND trim(last_error) = ''`,
	},
	{
		name:   "orphaned_step_reference",
		detail: "jobs pointing at a campaign step that no longer exists",
		query: `
			SELECT count(*) FROM scheduled_messages sm
			WHERE NOT EXISTS (SELECT 1 FROM campaign_steps cs WHERE cs.id = sm.campaign_step_id)`,
	},
	{
		name:   "orphaned_template_reference",
		detail: "jobs whose step points at a template that no longer exists",
		query: `
			SELECT count(*)
			FROM scheduled_messages sm
			JOIN campaign_steps cs ON cs.id = sm.campaign_step_id
			WHERE NOT EXISTS (
				SELECT 1 FROM message_templates t WHERE t.id = cs.message_template_id
			)`,
	},
	{
		name:   "invalid_scheduled_at",
		detail: "jobs with a missing or unparseable scheduled time",
		// Timestamps are stored as fixed-width UTC strings so they sort as text.
		// Anything that does not match that shape would sort unpredictably and
		// could be claimed at the wrong moment, or never.
		query: `
			SELECT count(*) FROM scheduled_messages
			WHERE scheduled_at IS NULL
			   OR trim(scheduled_at) = ''
			   OR date(replace(replace(scheduled_at, 'T', ' '), 'Z', '')) IS NULL`,
	},
}

// Consistency runs every check and reports the results.
//
// It reads only. Repair is reconciliation's job, and keeping the two apart
// means the report can be trusted as evidence of what the database actually
// contains.
func (s *Service) Consistency(ctx context.Context) (*ConsistencyReport, error) {
	report := &ConsistencyReport{
		Checks:  make([]CheckResult, 0, len(consistencyChecks)),
		Healthy: true,
	}

	for _, check := range consistencyChecks {
		var count int
		if err := s.repo.DB().QueryRow(ctx, check.query).Scan(&count); err != nil {
			return nil, fmt.Errorf("consistency check %s: %w", check.name, err)
		}

		report.Checks = append(report.Checks, CheckResult{
			Name:    check.name,
			Detail:  check.detail,
			Count:   count,
			Healthy: count == 0,
		})
		if count > 0 {
			report.Healthy = false
			report.Issues += count
		}
	}

	return report, nil
}
