package webhooks

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ayran/whatsapp-automation/internal/contacts"
	"github.com/ayran/whatsapp-automation/internal/conversations"
	"github.com/ayran/whatsapp-automation/internal/domain"
	"github.com/ayran/whatsapp-automation/internal/media"
	"github.com/ayran/whatsapp-automation/internal/realtime"
	"github.com/ayran/whatsapp-automation/internal/whatsapp/greenapi"
)

// Enricher pulls inbound attachments and contact avatars into local storage.
//
// Both are best-effort background work: the conversation is fully usable
// without them, so failures are logged and retried later rather than blocking
// message processing.
type Enricher struct {
	client   *greenapi.Client
	messages *conversations.Repository
	contacts *contacts.Repository
	media    *media.Service
	hub      *realtime.Hub
	log      *slog.Logger

	// maxDownloadBytes caps a single inbound attachment.
	maxDownloadBytes int64
	// avatarTTL is how long a cached avatar is trusted before refetching.
	avatarTTL time.Duration

	wg   sync.WaitGroup
	once sync.Once
}

func NewEnricher(
	client *greenapi.Client,
	messageRepo *conversations.Repository,
	contactRepo *contacts.Repository,
	mediaSvc *media.Service,
	hub *realtime.Hub,
	log *slog.Logger,
) *Enricher {
	return &Enricher{
		client:           client,
		messages:         messageRepo,
		contacts:         contactRepo,
		media:            mediaSvc,
		hub:              hub,
		log:              log.With(slog.String("component", "enrichment")),
		maxDownloadBytes: mediaSvc.Store().MaxBytes(),
		avatarTTL:        7 * 24 * time.Hour,
	}
}

func (e *Enricher) Start(ctx context.Context) {
	if e.client == nil || !e.client.Configured() {
		e.log.Info("enrichment disabled: provider is not configured")
		return
	}

	e.once.Do(func() {
		e.wg.Add(2)
		go e.mediaLoop(ctx)
		go e.avatarLoop(ctx)
		e.log.Info("enrichment workers started")
	})
}

func (e *Enricher) Wait() { e.wg.Wait() }

// mediaLoop captures inbound attachments before the provider's temporary
// download url expires.
func (e *Enricher) mediaLoop(ctx context.Context) {
	defer e.wg.Done()

	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.downloadPending(ctx)
		}
	}
}

func (e *Enricher) downloadPending(ctx context.Context) {
	pending, err := e.messages.PendingMediaDownloads(ctx, 10)
	if err != nil {
		e.log.Error("listing pending downloads failed", slog.String("error", err.Error()))
		return
	}

	for _, item := range pending {
		if ctx.Err() != nil {
			return
		}

		data, contentType, err := e.client.Download(ctx, item.URL, e.maxDownloadBytes)
		if err != nil {
			e.log.Warn("inbound media download failed",
				slog.String("message_id", item.MessageID.String()),
				slog.String("error", err.Error()))
			if markErr := e.messages.MarkDownloadFailed(ctx, item.MessageID, err.Error()); markErr != nil {
				e.log.Error("marking download failed", slog.String("error", markErr.Error()))
			}
			continue
		}

		mimeType := strings.TrimSpace(item.MimeType)
		if mimeType == "" {
			mimeType = contentType
		}

		name := item.FileName
		if strings.TrimSpace(name) == "" || filepath.Ext(name) == "" {
			name = "inbound" + media.ExtensionForMIME(mimeType)
		}

		file, err := e.media.SaveInbound(ctx, data, name, mimeType)
		if err != nil {
			e.log.Warn("storing inbound media failed",
				slog.String("message_id", item.MessageID.String()),
				slog.String("error", err.Error()))
			if markErr := e.messages.MarkDownloadFailed(ctx, item.MessageID, err.Error()); markErr != nil {
				e.log.Error("marking download failed", slog.String("error", markErr.Error()))
			}
			continue
		}

		if err := e.messages.AttachDownloadedMedia(ctx, item.MessageID, file.ID); err != nil {
			e.log.Error("attaching inbound media failed", slog.String("error", err.Error()))
			continue
		}

		e.log.Debug("inbound media stored",
			slog.String("message_id", item.MessageID.String()),
			slog.Int64("bytes", file.SizeBytes))
	}
}

// avatarLoop keeps contact profile pictures fresh.
//
// WhatsApp's own avatar urls are short-lived and often unreachable from a
// browser, so the image is copied into local storage and served from the
// platform's own domain.
func (e *Enricher) avatarLoop(ctx context.Context) {
	defer e.wg.Done()

	// A short warm-up avoids competing with startup work.
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}

	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.refreshAvatars(ctx)
		}
	}
}

func (e *Enricher) refreshAvatars(ctx context.Context) {
	stale, err := e.contacts.StaleAvatars(ctx, e.avatarTTL, 5)
	if err != nil {
		e.log.Error("listing stale avatars failed", slog.String("error", err.Error()))
		return
	}

	for _, contact := range stale {
		if ctx.Err() != nil {
			return
		}

		sourceURL, err := e.client.GetAvatar(ctx, contact.ChatID)
		if err != nil {
			e.log.Debug("avatar lookup failed",
				slog.String("contact_id", contact.ID.String()),
				slog.String("error", err.Error()))
			// Record the attempt so a contact without an avatar is not
			// retried on every tick.
			if markErr := e.contacts.UpdateAvatar(ctx, contact.ID, "", ""); markErr != nil {
				e.log.Error("recording avatar attempt failed", slog.String("error", markErr.Error()))
			}
			continue
		}

		if sourceURL == "" {
			if err := e.contacts.UpdateAvatar(ctx, contact.ID, "", ""); err != nil {
				e.log.Error("clearing avatar failed", slog.String("error", err.Error()))
			}
			continue
		}
		// Unchanged avatar: refresh the timestamp only.
		if sourceURL == contact.AvatarSourceURL && contact.AvatarURL != "" {
			if err := e.contacts.UpdateAvatar(ctx, contact.ID, contact.AvatarURL, sourceURL); err != nil {
				e.log.Error("touching avatar failed", slog.String("error", err.Error()))
			}
			continue
		}

		// Profile pictures are small; 4 MiB is a generous ceiling.
		data, contentType, err := e.client.Download(ctx, sourceURL, 4<<20)
		if err != nil {
			e.log.Debug("avatar download failed",
				slog.String("contact_id", contact.ID.String()),
				slog.String("error", err.Error()))
			if markErr := e.contacts.UpdateAvatar(ctx, contact.ID, "", sourceURL); markErr != nil {
				e.log.Error("recording avatar attempt failed", slog.String("error", markErr.Error()))
			}
			continue
		}

		ext := media.ExtensionForMIME(contentType)
		if ext == "" {
			ext = ".jpg"
		}

		file, err := e.media.SaveInbound(ctx, data, "avatar"+ext, contentType)
		if err != nil {
			e.log.Debug("storing avatar failed",
				slog.String("contact_id", contact.ID.String()),
				slog.String("error", err.Error()))
			if markErr := e.contacts.UpdateAvatar(ctx, contact.ID, "", sourceURL); markErr != nil {
				e.log.Error("recording avatar attempt failed", slog.String("error", markErr.Error()))
			}
			continue
		}

		localURL := e.media.Store().PublicURL(file.ID)
		if err := e.contacts.UpdateAvatar(ctx, contact.ID, localURL, sourceURL); err != nil {
			e.log.Error("saving avatar failed", slog.String("error", err.Error()))
			continue
		}

		if e.hub != nil {
			e.hub.Publish(realtime.Event{
				Type:      realtime.EventContactUpdated,
				ContactID: contact.ID.String(),
				Data:      map[string]any{"avatar_url": localURL},
			})
		}
	}
}

// RefreshContactProfile fetches the display name and avatar for one contact on
// demand, used by the "refresh" button in the chat header.
func (e *Enricher) RefreshContactProfile(ctx context.Context, contact *domain.Contact) (string, error) {
	if e.client == nil || !e.client.Configured() {
		return "", nil
	}

	info, err := e.client.GetContactInfo(ctx, contact.ChatID)
	if err != nil {
		return "", err
	}

	localURL := contact.AvatarURL
	if info.AvatarURL != "" && info.AvatarURL != contact.AvatarSourceURL {
		data, contentType, err := e.client.Download(ctx, info.AvatarURL, 4<<20)
		if err == nil {
			ext := media.ExtensionForMIME(contentType)
			if ext == "" {
				ext = ".jpg"
			}
			if file, saveErr := e.media.SaveInbound(ctx, data, "avatar"+ext, contentType); saveErr == nil {
				localURL = e.media.Store().PublicURL(file.ID)
			}
		}
	}

	if err := e.contacts.UpdateAvatar(ctx, contact.ID, localURL, info.AvatarURL); err != nil {
		return "", err
	}
	return localURL, nil
}
