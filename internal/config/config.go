// Package config loads runtime configuration from the process environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	App      App
	HTTP     HTTP
	Database Database
	Auth     Auth
	// GreenAPI and Outbound are the Primary account's settings, read from the
	// variables this service has always used. They are kept as their own
	// fields rather than folded into Instances because everything that is not
	// about a specific account -- the admin panel's provider check, the
	// documented defaults, the existing deployment -- refers to them.
	GreenAPI  GreenAPI
	Media     Media
	Scheduler Scheduler
	Outbound  Outbound

	// Instances is every WhatsApp account this process should serve, Primary
	// first. It always holds at least the Primary entry, even when no
	// credentials are configured for it, so that startup can report the
	// account as present-but-not-connected rather than missing.
	Instances []WhatsAppInstance
}

// WhatsAppInstance is one account's runtime configuration: which Green API
// instance backs it, and how fast it is allowed to send.
//
// Each account gets its own immutable copy. Nothing in the process mutates a
// live client's credentials, and no two accounts share a rate limiter, so a
// burst on one number cannot delay the other and a misconfigured second
// account cannot corrupt the first one's requests.
type WhatsAppInstance struct {
	// ID matches whatsapp_instances.id in the database; Key matches
	// whatsapp_instances.instance_key and is what the two are joined on.
	ID  int64
	Key string
	// Name and PhoneLabel are display-only, shown in logs and in the panel.
	Name       string
	PhoneLabel string
	// Enabled switches an account off without removing anything it owns: no
	// poller, no new sends, all of its data left exactly where it is.
	Enabled  bool
	GreenAPI GreenAPI
	Outbound Outbound
}

// Configured reports whether credentials are present for this account.
func (i WhatsAppInstance) Configured() bool {
	return strings.TrimSpace(i.GreenAPI.InstanceID) != "" && strings.TrimSpace(i.GreenAPI.Token) != ""
}

// Active reports whether the process should poll and send for this account.
func (i WhatsAppInstance) Active() bool { return i.Enabled && i.Configured() }

type App struct {
	Env             string
	DefaultTimezone string
	LogLevel        string
	LogFormat       string
	// SeedDefaultCampaign installs the Airan campaign when the database holds
	// no campaigns at all. It never modifies a database that already has one,
	// so leaving it on is safe on an existing installation.
	SeedDefaultCampaign bool
	// SeedCampaignActive makes that campaign go live immediately.
	//
	// Off by default. Every media step is seeded as text until its asset is
	// uploaded, so activating on first boot would send the script of a voice
	// note to real leads as a written message. Turning this on is a statement
	// that the assets are already in place.
	SeedCampaignActive bool
}

type HTTP struct {
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
	TrustedProxies  []string
	AllowedOrigins  []string
	WebDir          string
}

type Database struct {
	// Path is the SQLite file. Everything the application stores lives in it,
	// so backing the service up means copying this file (and its -wal
	// sidecar) while the service is stopped.
	Path            string
	MaxConns        int32
	BusyTimeout     time.Duration
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	AutoMigrate     bool
}

type Auth struct {
	SessionSecret     string
	SessionTTL        time.Duration
	SessionCookieName string
	CSRFCookieName    string
	SecureCookies     bool
	CookieDomain      string
	LoginMaxAttempts  int
	LoginLockWindow   time.Duration
	LoginLockDuration time.Duration
	BootstrapEmail    string
	BootstrapPassword string
}

type GreenAPI struct {
	InstanceID string
	Token      string
	BaseURL    string
	MediaURL   string
	Timeout    time.Duration
	// RateInterval is the minimum spacing between two outbound API calls.
	RateInterval time.Duration

	// ReceiveTimeout is how long the provider holds an inbound poll open
	// waiting for a notification before answering "nothing".
	ReceiveTimeout time.Duration
	// PollInterval is the pause after an empty queue before polling again.
	PollInterval time.Duration
}

type Media struct {
	StoragePath  string
	PublicURL    string
	MaxUploadMB  int64
	FFmpegPath   string
	FFprobePath  string
	VoiceBitrate string
}

type Scheduler struct {
	Enabled        bool
	Workers        int
	PollInterval   time.Duration
	BatchSize      int
	MaxAttempts    int
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	LockTimeout    time.Duration
	StaleJobTTL    time.Duration
	// ReconcileInterval is how often every running enrollment is checked
	// against its campaign's steps, and missing jobs created.
	//
	// This is a safety net, not the primary mechanism: steps reconcile the
	// moment they are edited. It exists because the primary mechanism is code,
	// and code has bugs — a step that loses its job for any reason at all is
	// repaired within one interval instead of never. A pass over a correct
	// campaign only reads, so the interval can be short.
	ReconcileInterval time.Duration
	// TriggerDelay is the floor applied to a step scheduled at trigger time, so
	// the first reply never lands in the same instant as the customer's own
	// message. It is a scheduled job, not a sleep: the queue holds the message
	// and a worker picks it up when it comes due.
	TriggerDelay time.Duration
	// Inbound provider notifications: how many workers apply them and how
	// many are claimed per batch.
	NotificationWorkers   int
	NotificationBatchSize int
}

// Outbound paces everything the platform sends. One WhatsApp account is one
// conversation partner as far as the provider is concerned, so the defaults
// serialise sending and space it out rather than pushing for throughput.
type Outbound struct {
	// Workers is how many messages may be in flight at once. Keep at 1 for a
	// single account.
	Workers int
	// MinDelay and MaxDelay bound the pause between two consecutive sends. The
	// actual pause is drawn from that range so a long queue does not go out on
	// a metronome.
	MinDelay time.Duration
	MaxDelay time.Duration
}

// Load reads configuration from the environment, applying defaults and
// validating anything the process cannot start without.
func Load() (*Config, error) {
	cfg := &Config{
		App: App{
			Env:                 envStr("APP_ENV", "development"),
			DefaultTimezone:     envStr("TIMEZONE", "Asia/Almaty"),
			LogLevel:            envStr("LOG_LEVEL", "info"),
			LogFormat:           envStr("LOG_FORMAT", "json"),
			SeedDefaultCampaign: envBool("SEED_DEFAULT_CAMPAIGN", true),
			SeedCampaignActive:  envBool("SEED_CAMPAIGN_ACTIVE", false),
		},
		HTTP: HTTP{
			Port:            envInt("PORT", 8086),
			ReadTimeout:     envDuration("HTTP_READ_TIMEOUT", 30*time.Second),
			WriteTimeout:    envDuration("HTTP_WRITE_TIMEOUT", 60*time.Second),
			IdleTimeout:     envDuration("HTTP_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout: envDuration("HTTP_SHUTDOWN_TIMEOUT", 25*time.Second),
			TrustedProxies:  envList("TRUSTED_PROXIES", nil),
			AllowedOrigins:  envList("ALLOWED_ORIGINS", nil),
			WebDir:          envStr("WEB_DIR", "./web"),
		},
		Database: Database{
			Path: envStr("DATABASE_PATH", "./data/whatsapp.db"),
			// SQLite serialises writes regardless of pool size; a handful of
			// connections is enough to keep readers moving without piling up
			// on the write lock.
			MaxConns:        int32(envInt("DATABASE_MAX_CONNS", 8)),
			BusyTimeout:     envDuration("DATABASE_BUSY_TIMEOUT", 10*time.Second),
			MaxConnLifetime: envDuration("DATABASE_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime: envDuration("DATABASE_CONN_IDLE", 30*time.Minute),
			AutoMigrate:     envBool("DATABASE_AUTO_MIGRATE", true),
		},
		Auth: Auth{
			SessionSecret:     envStr("SESSION_SECRET", ""),
			SessionTTL:        envDuration("SESSION_TTL", 12*time.Hour),
			SessionCookieName: envStr("SESSION_COOKIE_NAME", "wa_session"),
			CSRFCookieName:    envStr("CSRF_COOKIE_NAME", "wa_csrf"),
			SecureCookies:     envBool("SECURE_COOKIES", false),
			CookieDomain:      envStr("COOKIE_DOMAIN", ""),
			LoginMaxAttempts:  envInt("LOGIN_MAX_ATTEMPTS", 8),
			LoginLockWindow:   envDuration("LOGIN_LOCK_WINDOW", 15*time.Minute),
			LoginLockDuration: envDuration("LOGIN_LOCK_DURATION", 15*time.Minute),
			BootstrapEmail:    envStr("ADMIN_EMAIL", ""),
			BootstrapPassword: envStr("ADMIN_PASSWORD", ""),
		},
		GreenAPI: GreenAPI{
			InstanceID:   envStr("GREEN_API_INSTANCE_ID", ""),
			Token:        envStr("GREEN_API_TOKEN", ""),
			BaseURL:      strings.TrimRight(envStr("GREEN_API_URL", "https://api.green-api.com"), "/"),
			MediaURL:     strings.TrimRight(envStr("GREEN_API_MEDIA_URL", "https://media.green-api.com"), "/"),
			Timeout:      envDuration("GREEN_API_TIMEOUT", 45*time.Second),
			RateInterval: envDuration("GREEN_API_RATE_INTERVAL", 1100*time.Millisecond),

			ReceiveTimeout: envDuration("GREEN_API_RECEIVE_TIMEOUT", 20*time.Second),
			PollInterval:   time.Duration(envInt("GREEN_API_POLL_INTERVAL_MS", 1000)) * time.Millisecond,
		},
		Media: Media{
			StoragePath:  envStr("MEDIA_STORAGE_PATH", "./storage/media"),
			PublicURL:    strings.TrimRight(envStr("MEDIA_PUBLIC_URL", ""), "/"),
			MaxUploadMB:  int64(envInt("MEDIA_MAX_UPLOAD_MB", 64)),
			FFmpegPath:   envStr("FFMPEG_PATH", "ffmpeg"),
			FFprobePath:  envStr("FFPROBE_PATH", "ffprobe"),
			VoiceBitrate: envStr("VOICE_BITRATE", "32k"),
		},
		Scheduler: Scheduler{
			Enabled:      envBool("SCHEDULER_ENABLED", true),
			Workers:      envInt("SCHEDULER_WORKERS", 2),
			PollInterval: envDuration("SCHEDULER_POLL_INTERVAL", time.Second),
			BatchSize:    envInt("SCHEDULER_BATCH_SIZE", 25),
			MaxAttempts:  envInt("SCHEDULER_MAX_ATTEMPTS", 4),
			// 5s, 15s, 30s, 60s in the same spirit as the documented policy;
			// the backoff adds jitter around each step.
			RetryBaseDelay: envDuration("SCHEDULER_RETRY_BASE_DELAY", 10*time.Second),
			RetryMaxDelay:  envDuration("SCHEDULER_RETRY_MAX_DELAY", 30*time.Minute),
			// A send cannot legitimately outlive the provider timeout by much,
			// and this lease is also how long one stuck message may hold up the
			// rest of that contact's queue. Keep it tight.
			LockTimeout:           envDuration("SCHEDULER_LOCK_TIMEOUT", 2*time.Minute),
			StaleJobTTL:           envDuration("SCHEDULER_STALE_JOB_TTL", 2*time.Hour),
			ReconcileInterval:     envDuration("SCHEDULER_RECONCILE_INTERVAL", 30*time.Second),
			TriggerDelay:          time.Duration(envInt("TRIGGER_GREETING_DELAY_MS", 2000)) * time.Millisecond,
			NotificationWorkers:   envInt("GREEN_API_WORKERS", 5),
			NotificationBatchSize: envInt("GREEN_API_BATCH_SIZE", 50),
		},
		Outbound: Outbound{
			Workers:  envInt("WHATSAPP_SEND_WORKERS", 1),
			MinDelay: time.Duration(envInt("WHATSAPP_MIN_SEND_DELAY_MS", 2000)) * time.Millisecond,
			MaxDelay: time.Duration(envInt("WHATSAPP_MAX_SEND_DELAY_MS", 5000)) * time.Millisecond,
		},
	}

	instances, err := loadInstances(cfg)
	if err != nil {
		return nil, err
	}
	cfg.Instances = instances

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// maxInstances bounds how many accounts the environment may describe. It
// exists so a typo in a variable name cannot make startup scan forever, and it
// is deliberately generous: adding a third or fourth number needs no code
// change, only another block of _3 / _4 variables.
const maxInstances = 8

// instanceDefaults describes the accounts the database bootstraps in migration
// 0007. Slot 1 is the account this installation has always used, and its
// variables carry no suffix so an existing .env keeps working untouched.
var instanceDefaults = []struct {
	id   int64
	key  string
	name string
}{
	{1, "primary", "Primary WhatsApp"},
	{2, "secondary", "Secondary WhatsApp"},
}

// loadInstances reads one configuration block per account.
//
// Slot 1 reads GREEN_API_INSTANCE_ID / GREEN_API_TOKEN / WHATSAPP_MIN_SEND_DELAY_MS
// and so on. Every further slot reads the same names with an _N suffix:
// GREEN_API_INSTANCE_ID_2, WHATSAPP_MIN_SEND_DELAY_MS_2, and so on. Anything a
// slot does not set falls back to the Primary value, so a second number needs
// two variables in practice and can override the rest when its pacing should
// differ.
//
// Validation, per account beyond the first:
//
//	both id and token set    -> the account is configured and will run
//	neither set              -> the account is not configured; the process
//	                            starts without it and its stored data is left
//	                            untouched
//	exactly one of the two   -> a configuration error, refused at startup
//
// The last case is the one worth being strict about: half a credential pair is
// always a mistake, and starting anyway would leave an account that looks
// present in the panel and silently never sends.
func loadInstances(cfg *Config) ([]WhatsAppInstance, error) {
	var out []WhatsAppInstance

	for slot := 1; slot <= maxInstances; slot++ {
		suffix := suffixFor(slot)

		id := envStr("GREEN_API_INSTANCE_ID"+suffix, "")
		token := envStr("GREEN_API_TOKEN"+suffix, "")

		if slot > 1 {
			switch {
			case id == "" && token == "":
				// Not configured. Keep scanning: an operator may have set up
				// slot 3 and left slot 2 for later.
				continue
			case id == "" || token == "":
				return nil, fmt.Errorf(
					"invalid configuration:\n  - GREEN_API_INSTANCE_ID%s and GREEN_API_TOKEN%s must be set together (one of them is missing)",
					suffix, suffix)
			}
		}

		def := struct {
			id   int64
			key  string
			name string
		}{int64(slot), fmt.Sprintf("instance_%d", slot), fmt.Sprintf("WhatsApp #%d", slot)}
		if slot <= len(instanceDefaults) {
			def = instanceDefaults[slot-1]
		}

		inst := WhatsAppInstance{
			ID:         def.id,
			Key:        envStr("WHATSAPP_INSTANCE_KEY"+suffix, def.key),
			Name:       envStr("WHATSAPP_INSTANCE_NAME"+suffix, def.name),
			PhoneLabel: envStr("WHATSAPP_INSTANCE_PHONE"+suffix, ""),
			Enabled:    envBool("WHATSAPP_INSTANCE_ENABLED"+suffix, true),
			GreenAPI: GreenAPI{
				InstanceID: id,
				Token:      token,
				// The endpoints, timeouts and pacing default to Primary's, so a
				// second account on the same Green API region needs nothing but
				// its credentials.
				BaseURL:        strings.TrimRight(envStr("GREEN_API_URL"+suffix, cfg.GreenAPI.BaseURL), "/"),
				MediaURL:       strings.TrimRight(envStr("GREEN_API_MEDIA_URL"+suffix, cfg.GreenAPI.MediaURL), "/"),
				Timeout:        envDuration("GREEN_API_TIMEOUT"+suffix, cfg.GreenAPI.Timeout),
				RateInterval:   envDuration("GREEN_API_RATE_INTERVAL"+suffix, cfg.GreenAPI.RateInterval),
				ReceiveTimeout: envDuration("GREEN_API_RECEIVE_TIMEOUT"+suffix, cfg.GreenAPI.ReceiveTimeout),
				PollInterval: time.Duration(envInt("GREEN_API_POLL_INTERVAL_MS"+suffix,
					int(cfg.GreenAPI.PollInterval/time.Millisecond))) * time.Millisecond,
			},
			Outbound: Outbound{
				Workers: envInt("WHATSAPP_SEND_WORKERS"+suffix, cfg.Outbound.Workers),
				MinDelay: time.Duration(envInt("WHATSAPP_MIN_SEND_DELAY_MS"+suffix,
					int(cfg.Outbound.MinDelay/time.Millisecond))) * time.Millisecond,
				MaxDelay: time.Duration(envInt("WHATSAPP_MAX_SEND_DELAY_MS"+suffix,
					int(cfg.Outbound.MaxDelay/time.Millisecond))) * time.Millisecond,
			},
		}

		if slot == 1 {
			// Primary's settings are the ones validate() already checks and the
			// ones the panel reports, so they are taken verbatim rather than
			// re-read. This keeps exactly one source of truth for the account
			// that is already in production.
			inst.GreenAPI = cfg.GreenAPI
			inst.Outbound = cfg.Outbound
		}

		out = append(out, inst)
	}

	return out, nil
}

func (c *Config) validate() error {
	var problems []string

	if strings.TrimSpace(c.Database.Path) == "" {
		problems = append(problems, "DATABASE_PATH is required")
	}
	if len(c.Auth.SessionSecret) < 32 {
		problems = append(problems, "SESSION_SECRET is required and must be at least 32 characters")
	}
	if _, err := time.LoadLocation(c.App.DefaultTimezone); err != nil {
		problems = append(problems, fmt.Sprintf("TIMEZONE %q is not a valid IANA location", c.App.DefaultTimezone))
	}
	if c.HTTP.Port < 1 || c.HTTP.Port > 65535 {
		problems = append(problems, "PORT must be between 1 and 65535")
	}
	if c.Scheduler.Workers < 1 {
		problems = append(problems, "SCHEDULER_WORKERS must be at least 1")
	}
	if c.Scheduler.MaxAttempts < 1 {
		problems = append(problems, "SCHEDULER_MAX_ATTEMPTS must be at least 1")
	}
	if c.Media.MaxUploadMB < 1 {
		problems = append(problems, "MEDIA_MAX_UPLOAD_MB must be at least 1")
	}
	if c.Scheduler.NotificationWorkers < 1 {
		problems = append(problems, "GREEN_API_WORKERS must be at least 1")
	}
	// The outbound limits exist to keep automated sending calm and predictable.
	// A misconfiguration here is the one that gets a WhatsApp account into
	// trouble, so the bounds are enforced rather than clamped silently.
	if c.Outbound.Workers < 1 || c.Outbound.Workers > 4 {
		problems = append(problems, "WHATSAPP_SEND_WORKERS must be between 1 and 4; prefer 1 for a single account")
	}
	if c.Outbound.MinDelay < time.Second {
		problems = append(problems, "WHATSAPP_MIN_SEND_DELAY_MS must be at least 1000")
	}
	if c.Outbound.MaxDelay < c.Outbound.MinDelay {
		problems = append(problems, "WHATSAPP_MAX_SEND_DELAY_MS must be greater than or equal to WHATSAPP_MIN_SEND_DELAY_MS")
	}
	if c.Scheduler.TriggerDelay < time.Second || c.Scheduler.TriggerDelay > 5*time.Second {
		problems = append(problems, "TRIGGER_GREETING_DELAY_MS must be between 1000 and 5000")
	}
	// Inbound polling holds the connection open for ReceiveTimeout. A client
	// timeout below that aborts every poll before the provider can answer,
	// which looks exactly like the provider being down.
	if c.GreenAPI.Timeout <= c.GreenAPI.ReceiveTimeout {
		problems = append(problems, fmt.Sprintf(
			"GREEN_API_TIMEOUT (%s) must be greater than GREEN_API_RECEIVE_TIMEOUT (%s)",
			c.GreenAPI.Timeout, c.GreenAPI.ReceiveTimeout))
	}
	if c.IsProduction() && !c.Auth.SecureCookies {
		problems = append(problems, "SECURE_COOKIES must be true when APP_ENV=production")
	}

	// Additional accounts get the same limits as Primary. They are checked
	// separately, and named by their suffix, so a bad value in the second
	// block cannot be mistaken for a problem with the first: the account
	// already in production must never be blamed for a newcomer's typo.
	for i, inst := range c.Instances {
		if i == 0 {
			continue // Primary is covered by the checks above.
		}
		suffix := fmt.Sprintf("_%d", i+1)

		if inst.Outbound.Workers < 1 || inst.Outbound.Workers > 4 {
			problems = append(problems, "WHATSAPP_SEND_WORKERS"+suffix+" must be between 1 and 4; prefer 1 for a single account")
		}
		if inst.Outbound.MinDelay < time.Second {
			problems = append(problems, "WHATSAPP_MIN_SEND_DELAY_MS"+suffix+" must be at least 1000")
		}
		if inst.Outbound.MaxDelay < inst.Outbound.MinDelay {
			problems = append(problems, "WHATSAPP_MAX_SEND_DELAY_MS"+suffix+" must be greater than or equal to WHATSAPP_MIN_SEND_DELAY_MS"+suffix)
		}
		if inst.GreenAPI.Timeout <= inst.GreenAPI.ReceiveTimeout {
			problems = append(problems, fmt.Sprintf(
				"GREEN_API_TIMEOUT%s (%s) must be greater than GREEN_API_RECEIVE_TIMEOUT%s (%s)",
				suffix, inst.GreenAPI.Timeout, suffix, inst.GreenAPI.ReceiveTimeout))
		}
		if strings.TrimSpace(inst.Key) == "" {
			problems = append(problems, "WHATSAPP_INSTANCE_KEY"+suffix+" must not be blank")
		}
	}

	// Two accounts pointing at the same Green API instance would poll the same
	// queue and steal each other's notifications, which looks exactly like
	// messages going missing at random. It is always a copy-paste mistake.
	seen := map[string]int{}
	for i, inst := range c.Instances {
		id := strings.TrimSpace(inst.GreenAPI.InstanceID)
		if id == "" {
			continue
		}
		if prev, dup := seen[id]; dup {
			problems = append(problems, fmt.Sprintf(
				"GREEN_API_INSTANCE_ID%s repeats the value used by slot %d; each WhatsApp account needs its own Green API instance",
				suffixFor(i+1), prev+1))
			continue
		}
		seen[id] = i
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func (c *Config) IsProduction() bool { return c.App.Env == "production" }

// WhatsAppConfigured reports whether outbound Green API calls can be attempted
// on the Primary account. The platform still boots without credentials so the
// admin panel stays usable.
func (c *Config) WhatsAppConfigured() bool {
	return c.GreenAPI.InstanceID != "" && c.GreenAPI.Token != ""
}

// ActiveInstances returns the accounts that should poll and send.
func (c *Config) ActiveInstances() []WhatsAppInstance {
	var out []WhatsAppInstance
	for _, inst := range c.Instances {
		if inst.Active() {
			out = append(out, inst)
		}
	}
	return out
}

// InstanceByID finds a configured account by its database id.
func (c *Config) InstanceByID(id int64) (WhatsAppInstance, bool) {
	for _, inst := range c.Instances {
		if inst.ID == id {
			return inst, true
		}
	}
	return WhatsAppInstance{}, false
}

// suffixFor renders the environment-variable suffix for a 1-based slot.
func suffixFor(slot int) string {
	if slot <= 1 {
		return ""
	}
	return fmt.Sprintf("_%d", slot)
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}

func envList(key string, def []string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
