// Package analytics computes the dashboard counters and time series.
package analytics

import (
	"context"
	"fmt"
	"time"

	"github.com/ayran/whatsapp-automation/internal/storage/postgres"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

type Service struct {
	db *postgres.DB
}

func NewService(db *postgres.DB) *Service { return &Service{db: db} }

// Summary is the dashboard's headline block.
type Summary struct {
	TotalContacts     int `json:"total_contacts"`
	ActiveContacts    int `json:"active_contacts"`
	NewContactsToday  int `json:"new_contacts_today"`
	Unsubscribed      int `json:"unsubscribed_contacts"`
	MessagesSentToday int `json:"messages_sent_today"`
	MessagesRecvToday int `json:"messages_received_today"`
	ActiveCampaigns   int `json:"active_campaigns"`
	PendingJobs       int `json:"pending_scheduled_messages"`
	FailedJobs        int `json:"failed_messages"`
	CompletedRuns     int `json:"completed_automations"`
	UnreadChats       int `json:"unread_chats"`
	DeliveredToday    int `json:"delivered_today"`
	ReadToday         int `json:"read_today"`
}

// Summarize computes every dashboard counter in one round trip.
//
// "Today" is evaluated in the operator's timezone, not the server's, so the
// numbers match what the business day actually looked like.
func (s *Service) Summarize(ctx context.Context, tz string) (*Summary, error) {
	dayStart := timex.StartOfDayIn(time.Now().UTC(), tz)

	const query = `
		SELECT
			(SELECT count(*) FROM contacts),
			(SELECT count(*) FROM contacts WHERE status = 'ACTIVE'),
			(SELECT count(*) FROM contacts WHERE created_at >= $1),
			(SELECT count(*) FROM contacts WHERE opted_out),
			(SELECT count(*) FROM messages WHERE direction = 'OUTGOING' AND created_at >= $1 AND status <> 'FAILED'),
			(SELECT count(*) FROM messages WHERE direction = 'INCOMING' AND created_at >= $1),
			(SELECT count(*) FROM campaigns WHERE status = 'ACTIVE' AND archived_at IS NULL),
			(SELECT count(*) FROM scheduled_messages WHERE status = 'PENDING'),
			(SELECT count(*) FROM scheduled_messages WHERE status = 'FAILED'),
			(SELECT count(*) FROM campaign_contacts WHERE status = 'COMPLETED'),
			(SELECT count(*) FROM contacts WHERE unread_count > 0),
			(SELECT count(*) FROM messages WHERE direction = 'OUTGOING' AND delivered_at >= $1),
			(SELECT count(*) FROM messages WHERE direction = 'OUTGOING' AND read_at >= $1)`

	var out Summary
	err := s.db.Pool.QueryRow(ctx, query, dayStart).Scan(
		&out.TotalContacts, &out.ActiveContacts, &out.NewContactsToday, &out.Unsubscribed,
		&out.MessagesSentToday, &out.MessagesRecvToday, &out.ActiveCampaigns,
		&out.PendingJobs, &out.FailedJobs, &out.CompletedRuns, &out.UnreadChats,
		&out.DeliveredToday, &out.ReadToday,
	)
	if err != nil {
		return nil, fmt.Errorf("dashboard summary: %w", err)
	}
	return &out, nil
}

// SeriesPoint is one day of a time series.
type SeriesPoint struct {
	Date  string `json:"date"`
	Value int    `json:"value"`
}

// ContactsOverTime returns daily new-contact counts for the last n days.
func (s *Service) ContactsOverTime(ctx context.Context, tz string, days int) ([]SeriesPoint, error) {
	return s.dailySeries(ctx, tz, days, `
		SELECT (c.created_at AT TIME ZONE $1)::date AS day, count(*)
		FROM contacts c
		WHERE c.created_at >= $2
		GROUP BY day`)
}

// MessagesOverTime returns daily counts split by direction.
type MessageSeries struct {
	Incoming []SeriesPoint `json:"incoming"`
	Outgoing []SeriesPoint `json:"outgoing"`
}

func (s *Service) MessagesOverTime(ctx context.Context, tz string, days int) (*MessageSeries, error) {
	incoming, err := s.dailySeries(ctx, tz, days, `
		SELECT (m.created_at AT TIME ZONE $1)::date AS day, count(*)
		FROM messages m
		WHERE m.direction = 'INCOMING' AND m.created_at >= $2
		GROUP BY day`)
	if err != nil {
		return nil, err
	}

	outgoing, err := s.dailySeries(ctx, tz, days, `
		SELECT (m.created_at AT TIME ZONE $1)::date AS day, count(*)
		FROM messages m
		WHERE m.direction = 'OUTGOING' AND m.created_at >= $2
		GROUP BY day`)
	if err != nil {
		return nil, err
	}

	return &MessageSeries{Incoming: incoming, Outgoing: outgoing}, nil
}

// dailySeries runs a grouped query and fills gaps so the chart has one point
// per day even when nothing happened.
func (s *Service) dailySeries(ctx context.Context, tz string, days int, query string) ([]SeriesPoint, error) {
	if days <= 0 || days > 365 {
		days = 30
	}

	loc := timex.MustLocation(tz)
	now := time.Now().In(loc)
	start := timex.StartOfDayIn(now.UTC(), tz).Add(-time.Duration(days-1) * 24 * time.Hour)

	rows, err := s.db.Pool.Query(ctx, query, tz, start)
	if err != nil {
		return nil, fmt.Errorf("daily series: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int, days)
	for rows.Next() {
		var day time.Time
		var value int
		if err := rows.Scan(&day, &value); err != nil {
			return nil, err
		}
		counts[day.Format(timex.DateLayout)] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]SeriesPoint, 0, days)
	for i := 0; i < days; i++ {
		day := start.Add(time.Duration(i) * 24 * time.Hour).In(loc).Format(timex.DateLayout)
		out = append(out, SeriesPoint{Date: day, Value: counts[day]})
	}
	return out, nil
}

// DeliveryBreakdown counts outbound messages by final status.
type DeliveryBreakdown struct {
	Sent      int `json:"sent"`
	Delivered int `json:"delivered"`
	Read      int `json:"read"`
	Failed    int `json:"failed"`
	Pending   int `json:"pending"`
}

func (s *Service) Delivery(ctx context.Context, days int) (*DeliveryBreakdown, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	since := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour)

	const query = `
		SELECT
			count(*) FILTER (WHERE status = 'SENT'),
			count(*) FILTER (WHERE status = 'DELIVERED'),
			count(*) FILTER (WHERE status = 'READ'),
			count(*) FILTER (WHERE status = 'FAILED'),
			count(*) FILTER (WHERE status = 'PENDING')
		FROM messages
		WHERE direction = 'OUTGOING' AND created_at >= $1`

	var out DeliveryBreakdown
	if err := s.db.Pool.QueryRow(ctx, query, since).Scan(
		&out.Sent, &out.Delivered, &out.Read, &out.Failed, &out.Pending); err != nil {
		return nil, fmt.Errorf("delivery breakdown: %w", err)
	}
	return &out, nil
}

// CampaignStat summarises one campaign's funnel.
type CampaignStat struct {
	CampaignID   string  `json:"campaign_id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	Contacts     int     `json:"contacts"`
	Completed    int     `json:"completed"`
	Unsubscribed int     `json:"unsubscribed"`
	MessagesSent int     `json:"messages_sent"`
	Pending      int     `json:"pending"`
	Failed       int     `json:"failed"`
	Conversion   float64 `json:"completion_rate"`
}

func (s *Service) CampaignStats(ctx context.Context) ([]CampaignStat, error) {
	const query = `
		SELECT c.id, c.name, c.status,
			COALESCE(cc.total, 0), COALESCE(cc.completed, 0), COALESCE(cc.unsubscribed, 0),
			COALESCE(sm.sent, 0), COALESCE(sm.pending, 0), COALESCE(sm.failed, 0)
		FROM campaigns c
		LEFT JOIN (
			SELECT campaign_id,
				count(*) AS total,
				count(*) FILTER (WHERE status = 'COMPLETED') AS completed,
				count(*) FILTER (WHERE status = 'UNSUBSCRIBED') AS unsubscribed
			FROM campaign_contacts GROUP BY campaign_id
		) cc ON cc.campaign_id = c.id
		LEFT JOIN (
			SELECT campaign_id,
				count(*) FILTER (WHERE status = 'SENT') AS sent,
				count(*) FILTER (WHERE status = 'PENDING') AS pending,
				count(*) FILTER (WHERE status = 'FAILED') AS failed
			FROM scheduled_messages GROUP BY campaign_id
		) sm ON sm.campaign_id = c.id
		WHERE c.archived_at IS NULL
		ORDER BY COALESCE(cc.total, 0) DESC, c.created_at DESC`

	rows, err := s.db.Pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("campaign stats: %w", err)
	}
	defer rows.Close()

	var out []CampaignStat
	for rows.Next() {
		var st CampaignStat
		if err := rows.Scan(&st.CampaignID, &st.Name, &st.Status, &st.Contacts,
			&st.Completed, &st.Unsubscribed, &st.MessagesSent, &st.Pending, &st.Failed); err != nil {
			return nil, err
		}
		if st.Contacts > 0 {
			st.Conversion = float64(st.Completed) / float64(st.Contacts) * 100
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// TriggerStat counts how often each keyword brought someone into the funnel.
type TriggerStat struct {
	Keyword  string `json:"keyword"`
	Campaign string `json:"campaign"`
	Count    int    `json:"count"`
}

func (s *Service) TriggerStats(ctx context.Context, limit int) ([]TriggerStat, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	const query = `
		SELECT cc.trigger_keyword, c.name, count(*)
		FROM campaign_contacts cc
		JOIN campaigns c ON c.id = cc.campaign_id
		WHERE cc.trigger_keyword <> ''
		GROUP BY cc.trigger_keyword, c.name
		ORDER BY count(*) DESC
		LIMIT $1`

	rows, err := s.db.Pool.Query(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TriggerStat
	for rows.Next() {
		var st TriggerStat
		if err := rows.Scan(&st.Keyword, &st.Campaign, &st.Count); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
