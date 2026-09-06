//go:build integration

// Isolation between WhatsApp accounts.
//
// Everything in this file exists to prove one property: a message that arrives
// on one of the installation's numbers can only ever be answered by that
// number, and work queued for one number can only ever leave through it. The
// interesting failures are all silent ones -- a customer who wrote to the
// second number receiving the first number's webinar sequence, or a reminder
// going out from the wrong phone -- so each case is written to fail loudly if
// the scope is ever widened by accident.
package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ayran/whatsapp-automation/internal/campaigns"
	"github.com/ayran/whatsapp-automation/internal/domain"
)

// secondaryInstance is the second WhatsApp account, bootstrapped by migration
// 0007 alongside Primary.
const secondaryInstance int64 = 2

// TestMigrationBootstrapsBothAccounts is the foundation every other case here
// rests on: the accounts exist, with stable keys and stable ids, and they were
// created by the migration rather than by any test.
func TestMigrationBootstrapsBothAccounts(t *testing.T) {
	ctx := context.Background()

	rows, err := testDB.Query(ctx,
		`SELECT id, instance_key, enabled FROM whatsapp_instances ORDER BY id`)
	if err != nil {
		t.Fatalf("read whatsapp_instances: %v", err)
	}
	defer rows.Close()

	type account struct {
		id      int64
		key     string
		enabled bool
	}
	var got []account
	for rows.Next() {
		var a account
		if err := rows.Scan(&a.id, &a.key, &a.enabled); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	if len(got) < 2 {
		t.Fatalf("expected at least two whatsapp accounts, got %d", len(got))
	}
	if got[0].id != 1 || got[0].key != "primary" {
		t.Fatalf("account 1 must be the primary account, got id=%d key=%q", got[0].id, got[0].key)
	}
	if got[1].id != 2 || got[1].key != "secondary" {
		t.Fatalf("account 2 must be the secondary account, got id=%d key=%q", got[1].id, got[1].key)
	}
	for _, a := range got {
		if !a.enabled {
			t.Fatalf("account %q should be enabled on creation", a.key)
		}
	}
}

// TestExistingCampaignsBelongToPrimary is the backward-compatibility case.
//
// It writes a campaign the way a database that predates migration 0007 holds
// one -- with no owner at all -- and asserts that routing still finds it on
// Primary. That is what protects a row written by hand, or by a future
// migration, from becoming unreachable.
func TestExistingCampaignsBelongToPrimary(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	campaign := f.createCampaign(t, "Legacy", time.Now().UTC().Add(2*time.Hour), []int{-3600})
	f.addTrigger(t, campaign.ID, "LEGACY")

	// Strip the owner, reproducing a pre-0007 row.
	if _, err := testDB.Exec(ctx,
		`UPDATE campaigns SET whatsapp_instance_id = NULL WHERE id = $1`, campaign.ID); err != nil {
		t.Fatalf("clear owner: %v", err)
	}
	if _, err := testDB.Exec(ctx,
		`UPDATE campaign_triggers SET whatsapp_instance_id = NULL WHERE campaign_id = $1`, campaign.ID); err != nil {
		t.Fatalf("clear trigger owner: %v", err)
	}

	match, err := f.campaignSvc.MatchTrigger(ctx, nil, primaryInstance, "LEGACY")
	if err != nil {
		t.Fatalf("MatchTrigger: %v", err)
	}
	if match == nil {
		t.Fatal("a campaign with no stored owner must still route on the primary account")
	}
	if match.CampaignID != campaign.ID {
		t.Fatalf("matched the wrong campaign: %s", match.CampaignID)
	}

	// And it must not leak onto the other account.
	other, err := f.campaignSvc.MatchTrigger(ctx, nil, secondaryInstance, "LEGACY")
	if err != nil {
		t.Fatalf("MatchTrigger on secondary: %v", err)
	}
	if other != nil {
		t.Fatalf("a primary campaign matched on the secondary account: %s", other.CampaignID)
	}
}

// TestTriggerRoutingIsScopedToTheReceivingAccount is the headline case.
//
//	Primary campaign   trigger AAA
//	Secondary campaign trigger BBB
//
// BBB written to the Primary number must start nothing at all. The customer is
// talking to a number that has never heard of that word, and inventing an
// enrolment for them would put someone in a funnel they did not join.
func TestTriggerRoutingIsScopedToTheReceivingAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(2 * time.Hour)
	primary := f.createCampaignOn(t, primaryInstance, "Primary funnel", eventStart, []int{-3600})
	secondary := f.createCampaignOn(t, secondaryInstance, "Secondary funnel", eventStart, []int{-3600})

	f.addTrigger(t, primary.ID, "AAA")
	f.addTrigger(t, secondary.ID, "BBB")

	cases := []struct {
		name     string
		instance int64
		text     string
		want     *domain.Campaign // nil means "must not match"
	}{
		{"BBB on the primary number matches nothing", primaryInstance, "BBB", nil},
		{"AAA on the secondary number matches nothing", secondaryInstance, "AAA", nil},
		{"AAA on the primary number starts the primary funnel", primaryInstance, "AAA", primary},
		{"BBB on the secondary number starts the secondary funnel", secondaryInstance, "BBB", secondary},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			match, err := f.campaignSvc.MatchTrigger(ctx, nil, tc.instance, tc.text)
			if err != nil {
				t.Fatalf("MatchTrigger: %v", err)
			}
			switch {
			case tc.want == nil && match != nil:
				t.Fatalf("%q on account %d matched campaign %q; it must match nothing",
					tc.text, tc.instance, match.CampaignName)
			case tc.want != nil && match == nil:
				t.Fatalf("%q on account %d matched nothing; expected %q",
					tc.text, tc.instance, tc.want.Name)
			case tc.want != nil && match.CampaignID != tc.want.ID:
				t.Fatalf("%q on account %d matched %q; expected %q",
					tc.text, tc.instance, match.CampaignName, tc.want.Name)
			case tc.want != nil && match.WhatsAppInstanceID != tc.instance:
				t.Fatalf("match carries account %d; expected %d",
					match.WhatsAppInstanceID, tc.instance)
			}
		})
	}
}

// TestSameTriggerWordOnBothAccounts covers the case the old global uniqueness
// index made impossible.
//
// "START" is the obvious word for an opt-in, and both numbers should be free
// to use it: the number the customer wrote to has already resolved the
// ambiguity before routing begins.
func TestSameTriggerWordOnBothAccounts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(2 * time.Hour)
	primary := f.createCampaignOn(t, primaryInstance, "Primary start", eventStart, []int{-3600})
	secondary := f.createCampaignOn(t, secondaryInstance, "Secondary start", eventStart, []int{-3600})

	f.addTrigger(t, primary.ID, "START")

	// The same word on the other account must be accepted. Before the index
	// was rescoped this failed with ErrTriggerTaken.
	if _, err := f.campaignSvc.AddTrigger(ctx, secondary.ID, "START", "EXACT", true); err != nil {
		t.Fatalf("the same keyword must be allowed on a different account: %v", err)
	}

	primaryMatch, err := f.campaignSvc.MatchTrigger(ctx, nil, primaryInstance, "START")
	if err != nil {
		t.Fatalf("MatchTrigger primary: %v", err)
	}
	secondaryMatch, err := f.campaignSvc.MatchTrigger(ctx, nil, secondaryInstance, "START")
	if err != nil {
		t.Fatalf("MatchTrigger secondary: %v", err)
	}

	if primaryMatch == nil || primaryMatch.CampaignID != primary.ID {
		t.Fatalf("START on the primary number must start the primary campaign, got %+v", primaryMatch)
	}
	if secondaryMatch == nil || secondaryMatch.CampaignID != secondary.ID {
		t.Fatalf("START on the secondary number must start the secondary campaign, got %+v", secondaryMatch)
	}
}

// TestTriggerUniquenessStillHoldsWithinAnAccount is the other half of the
// rescoped index: routing inside one number must still be unambiguous.
func TestTriggerUniquenessStillHoldsWithinAnAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(2 * time.Hour)
	first := f.createCampaignOn(t, primaryInstance, "First", eventStart, []int{-3600})
	second := f.createCampaignOn(t, primaryInstance, "Second", eventStart, []int{-3600})

	f.addTrigger(t, first.ID, "JOIN")

	_, err := f.campaignSvc.AddTrigger(ctx, second.ID, "JOIN", "EXACT", true)
	if !errors.Is(err, campaigns.ErrTriggerTaken) {
		t.Fatalf("the same keyword twice on one account must be refused, got %v", err)
	}
}

// TestSamePhoneEnrolsOnBothAccountsIndependently covers a real customer who
// writes to both of the installation's numbers.
//
// They are one person and stay one contact row. What must not be shared is
// their position in a funnel: joining the first number's webinar cannot
// disqualify them from the second number's, and each enrolment must queue its
// own messages.
func TestSamePhoneEnrolsOnBothAccountsIndependently(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(3 * time.Hour)
	primary := f.createCampaignOn(t, primaryInstance, "Primary webinar", eventStart, []int{-3600})
	secondary := f.createCampaignOn(t, secondaryInstance, "Secondary webinar", eventStart, []int{-3600})
	f.addTrigger(t, primary.ID, "AAA")
	f.addTrigger(t, secondary.ID, "BBB")

	contact := f.createContact(t, "77001234567")

	primaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, primaryInstance, "AAA")
	if primaryMatch == nil {
		t.Fatal("primary trigger did not match")
	}
	primaryResult, err := f.campaignSvc.HandleTrigger(ctx, contact, primaryMatch, time.Now().UTC())
	if err != nil {
		t.Fatalf("HandleTrigger primary: %v", err)
	}
	if primaryResult.Action != campaigns.ActionEnrolled {
		t.Fatalf("expected enrolment on the primary account, got %s (%s)",
			primaryResult.Action, primaryResult.Reason)
	}

	secondaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, secondaryInstance, "BBB")
	if secondaryMatch == nil {
		t.Fatal("secondary trigger did not match")
	}
	secondaryResult, err := f.campaignSvc.HandleTrigger(ctx, contact, secondaryMatch, time.Now().UTC())
	if err != nil {
		t.Fatalf("HandleTrigger secondary: %v", err)
	}
	if secondaryResult.Action != campaigns.ActionEnrolled {
		t.Fatalf("an enrolment on one account must not block the other; got %s (%s)",
			secondaryResult.Action, secondaryResult.Reason)
	}

	// One contact row, two enrolments, two independent queues.
	if n := scalarCount(t, `SELECT count(*) FROM contacts`); n != 1 {
		t.Fatalf("expected the same person to stay one contact, got %d rows", n)
	}
	if n := scalarCount(t,
		`SELECT count(*) FROM campaign_contacts WHERE contact_id = $1`, contact.ID); n != 2 {
		t.Fatalf("expected one enrolment per account, got %d", n)
	}
	if n := scalarCount(t,
		`SELECT count(*) FROM scheduled_messages WHERE campaign_id = $1 AND status = 'PENDING'`,
		primary.ID); n == 0 {
		t.Fatal("the primary enrolment queued no messages")
	}
	if n := scalarCount(t,
		`SELECT count(*) FROM scheduled_messages WHERE campaign_id = $1 AND status = 'PENDING'`,
		secondary.ID); n == 0 {
		t.Fatal("the secondary enrolment queued no messages")
	}
}

// TestUnsubscribeOnOneAccountLeavesTheOtherRunning is the case the per-account
// opt-out exists for.
//
// A customer who sends STOP to the second number is talking about the second
// number's messages. Ending the first number's sequence would cancel a webinar
// they are still waiting for, and they would never find out why it stopped.
func TestUnsubscribeOnOneAccountLeavesTheOtherRunning(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(3 * time.Hour)
	primary := f.createCampaignOn(t, primaryInstance, "Primary webinar", eventStart, []int{-3600})
	secondary := f.createCampaignOn(t, secondaryInstance, "Secondary webinar", eventStart, []int{-3600})
	f.addTrigger(t, primary.ID, "AAA")
	f.addTrigger(t, secondary.ID, "BBB")

	contact := f.createContact(t, "77007654321")

	primaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, primaryInstance, "AAA")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, primaryMatch, time.Now().UTC()); err != nil {
		t.Fatalf("enrol on primary: %v", err)
	}
	secondaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, secondaryInstance, "BBB")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, secondaryMatch, time.Now().UTC()); err != nil {
		t.Fatalf("enrol on secondary: %v", err)
	}

	// The stop word must only be recognised on the account it reached.
	stopOnSecondary, err := f.campaignSvc.IsUnsubscribe(ctx, nil, contact.ID, secondaryInstance, "STOP")
	if err != nil {
		t.Fatalf("IsUnsubscribe: %v", err)
	}
	if !stopOnSecondary {
		t.Fatal("STOP is configured on the secondary campaign and must be recognised there")
	}

	if err := f.contactRepo.OptOutOn(ctx, nil, secondaryInstance, contact.ID, "sent STOP"); err != nil {
		t.Fatalf("record opt out: %v", err)
	}
	if err := f.campaignSvc.StopForContactOnInstance(ctx, contact.ID, secondaryInstance,
		domain.EnrollmentUnsubscribed, "contact sent an unsubscribe keyword"); err != nil {
		t.Fatalf("stop enrollments: %v", err)
	}

	// The opt-out itself is scoped.
	optedOutOnSecondary, err := f.contactRepo.IsOptedOutOn(ctx, nil, secondaryInstance, contact.ID)
	if err != nil {
		t.Fatalf("IsOptedOutOn secondary: %v", err)
	}
	optedOutOnPrimary, err := f.contactRepo.IsOptedOutOn(ctx, nil, primaryInstance, contact.ID)
	if err != nil {
		t.Fatalf("IsOptedOutOn primary: %v", err)
	}
	if !optedOutOnSecondary {
		t.Fatal("the contact must be recorded as unsubscribed on the account they wrote to")
	}
	if optedOutOnPrimary {
		t.Fatal("unsubscribing from one number must not unsubscribe from the other")
	}

	// The enrolments are scoped.
	if status := enrollmentStatus(t, secondary.ID, contact.ID); status != string(domain.EnrollmentUnsubscribed) {
		t.Fatalf("secondary enrolment should be UNSUBSCRIBED, got %s", status)
	}
	if status := enrollmentStatus(t, primary.ID, contact.ID); status != string(domain.EnrollmentActive) {
		t.Fatalf("primary enrolment must still be ACTIVE, got %s", status)
	}

	// And so is the queue.
	if n := scalarCount(t,
		`SELECT count(*) FROM scheduled_messages WHERE campaign_id = $1 AND status = 'PENDING'`,
		secondary.ID); n != 0 {
		t.Fatalf("secondary jobs should have been cancelled, %d still pending", n)
	}
	if n := scalarCount(t,
		`SELECT count(*) FROM scheduled_messages WHERE campaign_id = $1 AND status = 'PENDING'`,
		primary.ID); n == 0 {
		t.Fatal("the primary number's queued messages were cancelled by an unsubscribe meant for the other number")
	}
}

// TestJobsResolveTheirOwnAccount is the outbound half of isolation.
//
// Two numbers with work due in the same second: each job must report the
// account of the campaign that created it, because that is what the worker
// hands to the sender when it picks the client. It is also what makes a retry
// safe -- the account is re-derived on every attempt rather than captured when
// the job was queued.
func TestJobsResolveTheirOwnAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Both campaigns fire the same instant.
	eventStart := time.Now().UTC().Add(-time.Minute)
	primary := f.createCampaignOn(t, primaryInstance, "Primary due now", eventStart, []int{0})
	secondary := f.createCampaignOn(t, secondaryInstance, "Secondary due now", eventStart, []int{0})
	f.addTrigger(t, primary.ID, "AAA")
	f.addTrigger(t, secondary.ID, "BBB")

	contact := f.createContact(t, "77009998877")

	primaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, primaryInstance, "AAA")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, primaryMatch, time.Now().UTC()); err != nil {
		t.Fatalf("enrol on primary: %v", err)
	}
	secondaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, secondaryInstance, "BBB")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, secondaryMatch, time.Now().UTC()); err != nil {
		t.Fatalf("enrol on secondary: %v", err)
	}

	wantByCampaign := map[uuid.UUID]int64{
		primary.ID:   primaryInstance,
		secondary.ID: secondaryInstance,
	}

	rows, err := testDB.Query(ctx,
		`SELECT id, campaign_id FROM scheduled_messages WHERE status = 'PENDING'`)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}

	type job struct {
		id         uuid.UUID
		campaignID uuid.UUID
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.campaignID); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate: %v", err)
	}
	rows.Close()

	seen := map[int64]int{}
	for _, j := range jobs {
		want, ok := wantByCampaign[j.campaignID]
		if !ok {
			t.Fatalf("job %s belongs to an unexpected campaign %s", j.id, j.campaignID)
		}

		jc, err := f.jobRepo.LoadContext(ctx, j.id)
		if err != nil {
			t.Fatalf("LoadContext: %v", err)
		}
		if jc == nil {
			t.Fatalf("job %s has no context", j.id)
		}
		if jc.CampaignInstanceID != want {
			t.Fatalf("job for campaign %s resolved account %d; expected %d — this is a cross-send",
				j.campaignID, jc.CampaignInstanceID, want)
		}
		seen[want]++
	}

	if seen[primaryInstance] == 0 || seen[secondaryInstance] == 0 {
		t.Fatalf("expected at least one due job per account, got %v", seen)
	}
}

// TestPerAccountOptOutReachesTheSendPath proves the scheduler sees the scoped
// answer rather than the whole-person flag.
//
// The worker refuses to send when JobContext.ContactOptedOutHere is set, so
// this is the value that decides whether a queued message goes out. Reading
// the global flag here would cancel the other number's messages.
func TestPerAccountOptOutReachesTheSendPath(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(-time.Minute)
	primary := f.createCampaignOn(t, primaryInstance, "Primary due", eventStart, []int{0})
	secondary := f.createCampaignOn(t, secondaryInstance, "Secondary due", eventStart, []int{0})
	f.addTrigger(t, primary.ID, "AAA")
	f.addTrigger(t, secondary.ID, "BBB")

	contact := f.createContact(t, "77005554433")

	primaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, primaryInstance, "AAA")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, primaryMatch, time.Now().UTC()); err != nil {
		t.Fatalf("enrol on primary: %v", err)
	}
	secondaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, secondaryInstance, "BBB")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, secondaryMatch, time.Now().UTC()); err != nil {
		t.Fatalf("enrol on secondary: %v", err)
	}

	// They unsubscribe from the secondary number only, and the whole-person
	// flag is set exactly as production sets it today.
	if err := f.contactRepo.OptOutOn(ctx, nil, secondaryInstance, contact.ID, "sent STOP"); err != nil {
		t.Fatalf("record opt out: %v", err)
	}
	if err := f.contactRepo.SetOptOut(ctx, nil, contact.ID, true); err != nil {
		t.Fatalf("set whole-person flag: %v", err)
	}

	for _, tc := range []struct {
		name       string
		campaignID uuid.UUID
		wantOptOut bool
	}{
		{"the account they left refuses to send", secondary.ID, true},
		{"the account they stayed on keeps sending", primary.ID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var jobID uuid.UUID
			err := testDB.QueryRow(ctx,
				`SELECT id FROM scheduled_messages WHERE campaign_id = $1 LIMIT 1`,
				tc.campaignID).Scan(&jobID)
			if err != nil {
				t.Fatalf("find job: %v", err)
			}

			jc, err := f.jobRepo.LoadContext(ctx, jobID)
			if err != nil || jc == nil {
				t.Fatalf("LoadContext: %v", err)
			}

			if jc.ContactOptedOutHere != tc.wantOptOut {
				t.Fatalf("ContactOptedOutHere = %v, want %v", jc.ContactOptedOutHere, tc.wantOptOut)
			}
			// The whole-person flag is set either way; it is display state, and
			// the send path must not be reading it.
			if !jc.ContactOptedOut {
				t.Fatal("the whole-person flag should still be set, for the panel")
			}
		})
	}
}

// TestCampaignAccountSurvivesAnUpdate pins ownership down at the layer that
// would carry a change out. The API refuses one; this is the second lock.
func TestCampaignAccountSurvivesAnUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	campaign := f.createCampaignOn(t, secondaryInstance, "Secondary campaign",
		time.Now().UTC().Add(2*time.Hour), []int{-3600})

	moved := *campaign
	moved.Name = "Renamed"
	primaryOwner := primaryInstance
	moved.WhatsAppInstanceID = &primaryOwner

	if err := f.campaignRepo.Update(ctx, nil, &moved); err != nil {
		t.Fatalf("update campaign: %v", err)
	}

	reloaded, err := f.campaignRepo.GetByID(ctx, nil, campaign.ID)
	if err != nil || reloaded == nil {
		t.Fatalf("reload campaign: %v", err)
	}
	if got := domain.InstanceIDOrPrimary(reloaded.WhatsAppInstanceID); got != secondaryInstance {
		t.Fatalf("a campaign's account must not move on update: got %d, want %d", got, secondaryInstance)
	}
	if reloaded.Name != "Renamed" {
		t.Fatalf("the rest of the update should still apply, name is %q", reloaded.Name)
	}
}

// TestTriggersInheritTheirCampaignsAccount checks the one denormalised column
// in this design. The copy is written from the campaign inside the insert, so
// it cannot disagree with it.
func TestTriggersInheritTheirCampaignsAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	campaign := f.createCampaignOn(t, secondaryInstance, "Secondary campaign",
		time.Now().UTC().Add(2*time.Hour), []int{-3600})

	trigger, err := f.campaignSvc.AddTrigger(ctx, campaign.ID, "HELLO", "EXACT", true)
	if err != nil {
		t.Fatalf("add trigger: %v", err)
	}
	if got := domain.InstanceIDOrPrimary(trigger.WhatsAppInstanceID); got != secondaryInstance {
		t.Fatalf("trigger inherited account %d, want %d", got, secondaryInstance)
	}
}

func TestCampaignQueriesLoadTriggersWithAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	campaign := f.createCampaignOn(t, secondaryInstance, "Trigger scan campaign",
		time.Now().UTC().Add(2*time.Hour), []int{-3600})
	f.addTrigger(t, campaign.ID, "LOAD")

	list, err := f.campaignRepo.List(ctx, campaigns.ListFilter{IncludeArchived: true})
	if err != nil {
		t.Fatalf("list campaigns: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected one campaign in list, got %d", len(list))
	}
	if len(list[0].Triggers) != 1 {
		t.Fatalf("campaign list loaded %d triggers, want 1", len(list[0].Triggers))
	}
	if got := domain.InstanceIDOrPrimary(list[0].Triggers[0].WhatsAppInstanceID); got != secondaryInstance {
		t.Fatalf("listed trigger account %d, want %d", got, secondaryInstance)
	}

	full, err := f.campaignRepo.GetFull(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("get full campaign: %v", err)
	}
	if len(full.Triggers) != 1 {
		t.Fatalf("campaign detail loaded %d triggers, want 1", len(full.Triggers))
	}
	if got := domain.InstanceIDOrPrimary(full.Triggers[0].WhatsAppInstanceID); got != secondaryInstance {
		t.Fatalf("detail trigger account %d, want %d", got, secondaryInstance)
	}

	triggers, err := f.campaignRepo.ListTriggers(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("list triggers: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("ListTriggers loaded %d triggers, want 1", len(triggers))
	}
	if got := domain.InstanceIDOrPrimary(triggers[0].WhatsAppInstanceID); got != secondaryInstance {
		t.Fatalf("ListTriggers account %d, want %d", got, secondaryInstance)
	}
}

// TestReconciliationStillRepairsTheOtherAccount is the quiet failure this
// design is most likely to produce if the scope is ever widened by accident.
//
// The reconciler is the safety net that re-creates a job an enrolment lost. It
// used to skip anybody carrying the whole-person unsubscribe flag -- which,
// with two numbers, is set the moment a customer leaves *either* funnel. A
// customer who unsubscribed from the second number would then quietly stop
// being repaired on the first: their queue would decay, a step added later
// would never reach them, and nothing anywhere would report it.
func TestReconciliationStillRepairsTheOtherAccount(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	eventStart := time.Now().UTC().Add(3 * time.Hour)
	primary := f.createCampaignOn(t, primaryInstance, "Primary webinar", eventStart, []int{-3600})
	secondary := f.createCampaignOn(t, secondaryInstance, "Secondary webinar", eventStart, []int{-3600})
	f.addTrigger(t, primary.ID, "AAA")
	f.addTrigger(t, secondary.ID, "BBB")

	contact := f.createContact(t, "77003334455")

	primaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, primaryInstance, "AAA")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, primaryMatch, time.Now().UTC()); err != nil {
		t.Fatalf("enrol on primary: %v", err)
	}
	secondaryMatch, _ := f.campaignSvc.MatchTrigger(ctx, nil, secondaryInstance, "BBB")
	if _, err := f.campaignSvc.HandleTrigger(ctx, contact, secondaryMatch, time.Now().UTC()); err != nil {
		t.Fatalf("enrol on secondary: %v", err)
	}

	// They leave the secondary funnel, exactly as the inbound path records it.
	if err := f.contactRepo.OptOutOn(ctx, nil, secondaryInstance, contact.ID, "sent STOP"); err != nil {
		t.Fatalf("record opt out: %v", err)
	}
	if err := f.contactRepo.SetOptOut(ctx, nil, contact.ID, true); err != nil {
		t.Fatalf("set whole-person flag: %v", err)
	}

	reloaded, err := f.contactRepo.GetByID(ctx, contact.ID)
	if err != nil || reloaded == nil {
		t.Fatalf("reload contact: %v", err)
	}
	if !reloaded.OptedOut {
		t.Fatal("the whole-person flag should be set, as it is today")
	}

	enrolments, err := f.campaignRepo.EnrollmentsForReconcile(ctx, nil, nil)
	if err != nil {
		t.Fatalf("EnrollmentsForReconcile: %v", err)
	}

	var sawPrimary, sawSecondary bool
	for _, e := range enrolments {
		switch e.CampaignID {
		case primary.ID:
			sawPrimary = true
		case secondary.ID:
			sawSecondary = true
		}
	}
	if !sawPrimary {
		t.Fatal("the primary enrolment stopped being reconciled because of an unsubscribe on the other number")
	}
	if sawSecondary {
		t.Fatal("the secondary enrolment should be excluded: that is the number they left")
	}

	// And the same for back-filling a newly enabled step.
	active, err := f.campaignRepo.ActiveEnrollmentIDs(ctx, nil, primary.ID)
	if err != nil {
		t.Fatalf("ActiveEnrollmentIDs: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("a step added to the primary campaign must still reach this contact; got %d enrolments", len(active))
	}
}

// ------------------------------------------------------------- helpers --

// scalarCount runs a counting query. seed_test.go already owns countRows for
// whole-table counts; this one takes a query with arguments.
func scalarCount(t *testing.T, query string, args ...any) int {
	t.Helper()

	var n int
	if err := testDB.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func enrollmentStatus(t *testing.T, campaignID, contactID any) string {
	t.Helper()

	var status string
	err := testDB.QueryRow(context.Background(),
		`SELECT status FROM campaign_contacts WHERE campaign_id = $1 AND contact_id = $2`,
		campaignID, contactID).Scan(&status)
	if err != nil {
		t.Fatalf("read enrollment status: %v", err)
	}
	return status
}
