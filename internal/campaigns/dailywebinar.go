package campaigns

import (
	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/scheduler"
)

// The daily webinar sequence.
//
// A daily recurring webinar is an event that happens every day. It is not a
// broadcast that goes out every day. The distinction is the whole feature:
//
//	daily webinar + a new participant  = one sequence
//	daily webinar + everyone who ever  = a system that spams its own customers
//	                joined
//
// Migration 0005 already made the anchor per-enrolment: a contact belongs to
// the one occurrence that was current when they entered, and it is written
// once and never rolled forward. That is what keeps tomorrow's webinar from
// re-anchoring yesterday's contacts, and it is left exactly as it is.
//
// What is added here is the other half — the case where a contact's enrolment
// is deliberately re-run. A campaign configured with RESTART starts a new run
// when an existing participant sends the trigger again, and a new run means a
// new run_number, which means the queue's unique constraint no longer stands in
// the way: the whole sequence would be created a second time, on a later
// webinar. For a one-time campaign that is precisely what RESTART is for. For a
// daily webinar it is the bug the customer reported, because "they wrote to us
// again" is not "they are a new lead".
//
// So the rule is stated per step, by the operator, and enforced here:
//
//	a step marked include_in_daily_webinar is delivered to a contact once,
//	for the occurrence their enrolment is pinned to, and never again.
//
// Steps without the flag are untouched. A greeting that answers the contact's
// own message *should* answer it again if they write again; a post-webinar
// follow-up is not part of the webinar sequence. Only the operator knows which
// is which, so only the operator decides.

// DailySequence returns the steps that make up a campaign's daily webinar
// sequence, in campaign order.
//
// The count is whatever the operator marked. Seven is the shape of the Airan
// funnel, not a constant anywhere in this system: five, eight, ten and none all
// work, and none is the default.
func DailySequence(steps []domain.CampaignStep) []domain.CampaignStep {
	out := make([]domain.CampaignStep, 0, len(steps))
	for _, step := range steps {
		if step.IncludeInDailyWebinar {
			out = append(out, step)
		}
	}
	return out
}

// DailySequenceConsumed reports whether this enrolment has already been through
// the campaign's daily webinar sequence.
//
// "Been through" means a step of the sequence was actually delivered on an
// earlier run. Three conditions, each of which matters:
//
//   - the campaign is a daily recurring one. A one-time campaign has no daily
//     sequence, so RESTART keeps working there exactly as it always has.
//   - the enrolment is on a later run than the delivery. Without this the check
//     would fire in the middle of a contact's own first sequence — two messages
//     sent at 16:00 and 18:00 would cancel the five still to come — which is
//     the opposite of the intent. run_number only advances on a restart, so a
//     contact who has never restarted can never be caught by this.
//   - the earlier delivery is SENT. A step that was recorded as skipped, or
//     that was still queued when the restart cancelled it, was never received;
//     a contact who restarts before anything went out is genuinely starting,
//     and gets their one sequence.
//
// It reads jobs the caller has already loaded, so the periodic sweep pays no
// extra query for it.
func DailySequenceConsumed(
	campaign *domain.Campaign, steps []domain.CampaignStep,
	jobs []scheduler.ExistingJob, runNumber int,
) bool {
	if campaign == nil || !campaign.IsDailyRecurring || runNumber <= 1 {
		return false
	}

	daily := make(map[uuid.UUID]struct{}, len(steps))
	for _, step := range steps {
		if step.IncludeInDailyWebinar {
			daily[step.ID] = struct{}{}
		}
	}
	if len(daily) == 0 {
		return false
	}

	for _, job := range jobs {
		if job.RunNumber >= runNumber || job.Status != domain.JobSent {
			continue
		}
		if _, ok := daily[job.StepID]; ok {
			return true
		}
	}
	return false
}
