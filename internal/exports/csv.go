// Package exports streams filtered data out of the platform as CSV.
package exports

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"time"

	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/pkg/phone"
	"github.com/ayran/whatsapp-automation/pkg/timex"
)

type Service struct {
	contacts *contacts.Repository
}

func NewService(contactRepo *contacts.Repository) *Service {
	return &Service{contacts: contactRepo}
}

var contactHeader = []string{
	"phone",
	"name",
	"push_name",
	"campaign",
	"trigger",
	"status",
	"opted_out",
	"first_contact",
	"last_activity",
	"messages_received",
	"messages_sent",
	"created_at",
}

// Contacts writes the filtered contact set as CSV.
//
// Rows are streamed straight from the database cursor rather than collected in
// memory first, so exporting a large audience does not scale memory with the
// audience size.
func (s *Service) Contacts(ctx context.Context, w io.Writer, filter contacts.Filter, tz string) (int, error) {
	// Excel on Windows assumes the system code page unless a UTF-8 BOM is
	// present, which mangles Kazakh and Cyrillic names.
	if _, err := w.Write([]byte{0xEF, 0xBB, 0xBF}); err != nil {
		return 0, fmt.Errorf("write bom: %w", err)
	}

	writer := csv.NewWriter(w)
	defer writer.Flush()

	if err := writer.Write(contactHeader); err != nil {
		return 0, fmt.Errorf("write header: %w", err)
	}

	rows, err := s.contacts.ExportRows(ctx, filter)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var (
			phoneDigits    string
			name           string
			pushName       string
			campaign       string
			trigger        string
			status         string
			optedOut       bool
			firstContactAt *time.Time
			lastActivityAt *time.Time
			incoming       int
			outgoing       int
			createdAt      time.Time
		)

		if err := rows.Scan(&phoneDigits, &name, &pushName, &campaign, &trigger,
			&status, &optedOut, &firstContactAt, &lastActivityAt,
			&incoming, &outgoing, &createdAt); err != nil {
			return count, fmt.Errorf("scan export row: %w", err)
		}

		record := []string{
			phone.Display(phoneDigits),
			name,
			pushName,
			campaign,
			trigger,
			status,
			boolText(optedOut),
			formatTime(firstContactAt, tz),
			formatTime(lastActivityAt, tz),
			fmt.Sprint(incoming),
			fmt.Sprint(outgoing),
			timex.FormatIn(createdAt, tz, "2006-01-02 15:04:05"),
		}

		if err := writer.Write(record); err != nil {
			return count, fmt.Errorf("write row: %w", err)
		}
		count++

		// Flush periodically so the browser starts receiving the file
		// immediately instead of waiting for the whole result set.
		if count%500 == 0 {
			writer.Flush()
			if err := writer.Error(); err != nil {
				return count, err
			}
		}
	}

	if err := rows.Err(); err != nil {
		return count, err
	}

	writer.Flush()
	return count, writer.Error()
}

// Filename builds a timestamped download name.
func Filename(prefix, tz string) string {
	stamp := timex.FormatIn(time.Now().UTC(), tz, "2006-01-02_1504")
	return fmt.Sprintf("%s_%s.csv", prefix, stamp)
}

func formatTime(t *time.Time, tz string) string {
	if t == nil {
		return ""
	}
	return timex.FormatIn(*t, tz, "2006-01-02 15:04:05")
}

func boolText(v bool) string {
	if v {
		return "иә"
	}
	return "жоқ"
}
