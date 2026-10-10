//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ayran/whatsapp-automation/pkg/timex"
)

func TestScheduled1700AlmatyIsClaimedAt1200UTC(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	loc := timex.MustLocation("Asia/Almaty")
	tomorrow := time.Now().In(loc).AddDate(0, 0, 1)
	event := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 21, 0, 0, 0, loc).UTC()
	due := event.Add(-4 * time.Hour)
	campaign := f.createCampaign(t, "17:00 schedule", event, []int{-14400})
	f.addTrigger(t, campaign.ID, "JOIN")
	contact := f.createContact(t, "77015555555")
	match, err := f.campaignSvc.MatchTrigger(ctx, nil, primaryInstance, "JOIN")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, due.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	jobs := allJobs(t, f, enrollmentOf(t, f, campaign.ID, contact.ID).ID)
	if len(jobs) != 1 || !jobs[0].ScheduledAt.Equal(due) || due.Hour() != 12 {
		t.Fatalf("schedule=%+v expected %v", jobs, due)
	}
	before, err := f.jobRepo.Claim(ctx, "worker", 1, due.Add(-time.Nanosecond), due.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 0 {
		t.Fatal("message claimed before 17:00")
	}
	at, err := f.jobRepo.Claim(ctx, "worker", 1, due, due.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(at) != 1 || at[0].ID != jobs[0].ID {
		t.Fatalf("due message not claimed: %+v", at)
	}
}

func TestSameContactQueuesOnDifferentInstancesDoNotBlockEachOther(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	contact := f.createContact(t, "77015555555")
	for instance := int64(1); instance <= 2; instance++ {
		name := fmt.Sprintf("instance-%d", instance)
		campaign := f.createCampaignOn(t, instance, name, now.Add(time.Hour), []int{-60})
		f.addTrigger(t, campaign.ID, name)
		match, err := f.campaignSvc.MatchTrigger(ctx, nil, instance, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.campaignSvc.HandleTrigger(ctx, contact, match, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := testDB.Exec(ctx, `UPDATE scheduled_messages SET scheduled_at = $1`, now); err != nil {
		t.Fatal(err)
	}
	first, err := f.jobRepo.Claim(ctx, "first", 1, now, now.Add(-time.Minute))
	if err != nil || len(first) != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := f.jobRepo.Claim(ctx, "second", 1, now, now.Add(-time.Minute))
	if err != nil || len(second) != 1 {
		t.Fatalf("other instance blocked: %+v err=%v", second, err)
	}
	if first[0].CampaignID == second[0].CampaignID {
		t.Fatal("same account claimed twice")
	}
}
