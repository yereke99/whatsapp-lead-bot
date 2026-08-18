// Formatting helpers and the Kazakh label vocabulary used across the UI.

let appTimezone = 'Asia/Almaty';

export function setTimezone(tz) {
  if (tz) appTimezone = tz;
}

export function timezone() {
  return appTimezone;
}

function formatter(options) {
  return new Intl.DateTimeFormat('kk-KZ', { timeZone: appTimezone, ...options });
}

export function formatDateTime(value) {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '—';
  return formatter({
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit',
  }).format(date).replace(',', '');
}

export function formatDate(value) {
  if (!value) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '—';
  return formatter({ year: 'numeric', month: '2-digit', day: '2-digit' }).format(date);
}

export function formatTime(value, withSeconds = false) {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  return formatter({
    hour: '2-digit',
    minute: '2-digit',
    ...(withSeconds ? { second: '2-digit' } : {}),
  }).format(date);
}

// dateInputValue converts an instant to the YYYY-MM-DD a date input expects,
// expressed in the application timezone rather than the browser's.
export function dateInputValue(value) {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  const parts = formatter({ year: 'numeric', month: '2-digit', day: '2-digit' })
    .formatToParts(date)
    .reduce((acc, part) => ({ ...acc, [part.type]: part.value }), {});
  return `${parts.year}-${parts.month}-${parts.day}`;
}

export function timeInputValue(value) {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';
  const parts = formatter({ hour: '2-digit', minute: '2-digit', hour12: false })
    .formatToParts(date)
    .reduce((acc, part) => ({ ...acc, [part.type]: part.value }), {});
  return `${parts.hour}:${parts.minute}`;
}

// relativeTime renders a compact "how long ago" label for chat lists.
export function relativeTime(value) {
  if (!value) return '';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';

  const diffMs = Date.now() - date.getTime();
  const diffMin = Math.floor(diffMs / 60000);

  if (diffMin < 1) return 'қазір';
  if (diffMin < 60) return `${diffMin} мин`;

  const isToday = dateInputValue(date) === dateInputValue(new Date());
  if (isToday) return formatTime(date);

  const yesterday = new Date(Date.now() - 86400000);
  if (dateInputValue(date) === dateInputValue(yesterday)) return 'кеше';

  const diffDays = Math.floor(diffMin / 1440);
  if (diffDays < 7) return `${diffDays} күн`;

  return formatter({ day: '2-digit', month: '2-digit' }).format(date);
}

export function dayLabel(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '';

  const today = dateInputValue(new Date());
  const target = dateInputValue(date);
  if (target === today) return 'Бүгін';

  const yesterday = dateInputValue(new Date(Date.now() - 86400000));
  if (target === yesterday) return 'Кеше';

  return formatter({ day: 'numeric', month: 'long', year: 'numeric' }).format(date);
}

export function formatPhone(digits) {
  if (!digits) return '';
  const clean = String(digits).replace(/\D/g, '');
  if (clean.length === 11 && (clean[0] === '7' || clean[0] === '8')) {
    return `+${clean[0]} ${clean.slice(1, 4)} ${clean.slice(4, 7)} ${clean.slice(7, 9)} ${clean.slice(9, 11)}`;
  }
  return `+${clean}`;
}

export function formatBytes(bytes) {
  if (!bytes) return '0 Б';
  const units = ['Б', 'КБ', 'МБ', 'ГБ'];
  const index = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
  const value = bytes / 1024 ** index;
  return `${value.toFixed(index === 0 ? 0 : 1)} ${units[index]}`;
}

export function formatDuration(ms) {
  if (!ms) return '';
  const total = Math.round(ms / 1000);
  const minutes = Math.floor(total / 60);
  const seconds = total % 60;
  return `${minutes}:${String(seconds).padStart(2, '0')}`;
}

export function formatNumber(value) {
  return new Intl.NumberFormat('kk-KZ').format(value ?? 0);
}

// offsetLabel turns a signed second offset into Kazakh relative wording.
export function offsetLabel(seconds) {
  if (seconds === 0) return 'Дәл басталғанда';

  const before = seconds < 0;
  let rest = Math.abs(seconds);

  const hours = Math.floor(rest / 3600);
  rest -= hours * 3600;
  const minutes = Math.floor(rest / 60);
  const secs = rest - minutes * 60;

  const parts = [];
  if (hours) parts.push(`${hours} сағат`);
  if (minutes) parts.push(`${minutes} минут`);
  if (secs) parts.push(`${secs} секунд`);

  const text = parts.join(' ') || '0 секунд';
  return before ? `${text} бұрын` : `${text} кейін`;
}

// parseOffset reads the composite hours/minutes/seconds form back into a
// signed second count.
export function buildOffsetSeconds(direction, hours, minutes, seconds) {
  const total = (Number(hours) || 0) * 3600 + (Number(minutes) || 0) * 60 + (Number(seconds) || 0);
  return direction === 'after' ? total : -total;
}

export function splitOffset(seconds) {
  const direction = seconds > 0 ? 'after' : 'before';
  let rest = Math.abs(seconds);
  const hours = Math.floor(rest / 3600);
  rest -= hours * 3600;
  const minutes = Math.floor(rest / 60);
  return { direction, hours, minutes, seconds: rest - minutes * 60 };
}

// delayLabel renders a trigger-relative delay, where the offset counts forward
// from the customer's own message rather than from a fixed clock time.
export function delayLabel(seconds) {
  const total = Math.max(0, Number(seconds) || 0);
  if (total === 0) return 'бірден';

  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const secs = total % 60;

  const parts = [];
  if (hours) parts.push(`${hours} сағат`);
  if (minutes) parts.push(`${minutes} минут`);
  if (secs) parts.push(`${secs} секунд`);
  return parts.join(' ');
}

export function splitDelay(seconds) {
  const total = Math.max(0, Number(seconds) || 0);
  const hours = Math.floor(total / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  return { hours, minutes, seconds: total % 60 };
}

export function buildDelaySeconds(hours, minutes, seconds) {
  return (Number(hours) || 0) * 3600 + (Number(minutes) || 0) * 60 + (Number(seconds) || 0);
}

// campaignAnchor is the instant a campaign's event-anchored steps are measured
// from, for display and for editing.
//
// A daily recurring campaign is anchored to the webinar that is coming, which
// the server derives and sends as next_occurrence_at. A one-time campaign is
// anchored to its own event start. The server uses exactly the same rule when
// it converts an entered clock time back into an offset, so what the operator
// reads is what the operator gets.
export function campaignAnchor(campaign) {
  if (campaign?.is_daily_recurring && campaign.next_occurrence_at) {
    return campaign.next_occurrence_at;
  }
  return campaign?.event_start_at || null;
}

// stepRunAt resolves an event-anchored step to the instant it will be sent.
// Trigger-anchored steps have no single answer — each contact gets their own —
// so they return null and are described by their delay instead.
export function stepRunAt(campaign, step) {
  const anchor = campaignAnchor(campaign);
  if (!anchor) return null;
  if (step.schedule_kind === 'ON_TRIGGER') return null;
  return new Date(new Date(anchor).getTime() + (step.offset_seconds || 0) * 1000);
}

// formatInZone renders an instant in the campaign's own timezone. The server
// may run anywhere, so the browser's local zone is never used for campaign
// times.
export function formatInZone(instant, timeZone, options) {
  if (!instant) return '';
  return new Intl.DateTimeFormat('kk-KZ', { timeZone, hour12: false, ...options }).format(instant);
}

// dayInZone renders a calendar day as dd.MM.yyyy, which is the form used
// throughout the panel. It is assembled from parts rather than left to a
// locale, whose date order varies by runtime.
export function dayInZone(instant, timeZone) {
  if (!instant) return '';
  const parts = new Intl.DateTimeFormat('en-GB', {
    timeZone, year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(instant);
  const at = Object.fromEntries(parts.map((p) => [p.type, p.value]));
  return `${at.day}.${at.month}.${at.year}`;
}

// dateInZone and timeInZone produce the values an <input type="date"> and
// <input type="time"> expect, in the campaign's timezone rather than the
// browser's.
export function dateInZone(instant, timeZone) {
  if (!instant) return '';
  const parts = new Intl.DateTimeFormat('en-CA', {
    timeZone, year: 'numeric', month: '2-digit', day: '2-digit',
  }).formatToParts(instant);
  const lookup = Object.fromEntries(parts.map((p) => [p.type, p.value]));
  return `${lookup.year}-${lookup.month}-${lookup.day}`;
}

export function timeInZone(instant, timeZone) {
  if (!instant) return '';
  return new Intl.DateTimeFormat('en-GB', {
    timeZone, hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  }).format(instant);
}

// ------------------------------------------------------------- vocabulary --

export const CONTACT_STATUS = {
  NEW:          { label: 'Жаңа',            tone: 'info' },
  ACTIVE:       { label: 'Белсенді',        tone: 'success' },
  COMPLETED:    { label: 'Аяқталды',        tone: 'neutral' },
  UNSUBSCRIBED: { label: 'Жазылымнан шықты',tone: 'warn' },
  BLOCKED:      { label: 'Бұғатталған',     tone: 'danger' },
  ERROR:        { label: 'Қате',            tone: 'danger' },
};

export const CAMPAIGN_STATUS = {
  DRAFT:     { label: 'Жоба',      tone: 'neutral' },
  ACTIVE:    { label: 'Белсенді',  tone: 'success' },
  PAUSED:    { label: 'Кідіртілді',tone: 'warn' },
  COMPLETED: { label: 'Аяқталды',  tone: 'info' },
  ARCHIVED:  { label: 'Мұрағат',   tone: 'neutral' },
};

export const JOB_STATUS = {
  PENDING:    { label: 'Күтуде',      tone: 'info' },
  PROCESSING: { label: 'Жіберілуде',  tone: 'warn' },
  SENT:       { label: 'Жіберілді',   tone: 'success' },
  FAILED:     { label: 'Қате',        tone: 'danger' },
  CANCELLED:  { label: 'Тоқтатылды',  tone: 'neutral' },
};

export const MESSAGE_STATUS = {
  PENDING:   { label: 'Жіберілуде', tone: 'neutral' },
  SENT:      { label: 'Жіберілді',  tone: 'info' },
  DELIVERED: { label: 'Жеткізілді', tone: 'info' },
  READ:      { label: 'Оқылды',     tone: 'success' },
  FAILED:    { label: 'Қате',       tone: 'danger' },
  RECEIVED:  { label: 'Қабылданды', tone: 'neutral' },
};

export const TEMPLATE_TYPE = {
  TEXT:               'Мәтін',
  IMAGE:              'Сурет',
  IMAGE_WITH_CAPTION: 'Сурет + мәтін',
  VIDEO:              'Бейне',
  VIDEO_WITH_CAPTION: 'Бейне + мәтін',
  AUDIO:              'Аудио',
  VOICE:              'Дауыстық хабарлама',
  DOCUMENT:           'Құжат',
};

export const MEDIA_KIND = {
  IMAGE:    'Сурет',
  VIDEO:    'Бейне',
  AUDIO:    'Аудио',
  VOICE:    'Дауыстық',
  DOCUMENT: 'Құжат',
};

export const MATCH_MODE = {
  EXACT:       'Дәл сәйкестік',
  STARTS_WITH: 'Осыдан басталады',
  CONTAINS:    'Құрамында бар',
};

// How a queued message finds its moment. RELATIVE_TO_EVENT is a fixed point on
// the calendar shared by every contact; ON_TRIGGER is a delay counted from
// each contact's own message.
export const SCHEDULE_KIND = {
  RELATIVE_TO_EVENT: 'Нақты күн мен уақыт',
  ON_TRIGGER:        'Триггерден кейінгі кідіріс',
};

export const RESUME_POLICY = {
  SKIP_EXPIRED:    'Өтіп кеткендерді жіберме',
  SEND_NEXT_VALID: 'Соңғы өтіп кеткенін жібер',
};

export const EXISTING_BEHAVIOR = {
  IGNORE:          'Елемеу (қайта жібермеу)',
  RESTART:         'Кампанияны қайта бастау',
  CONTINUE:        'Ағымдағы кампанияны жалғастыру',
  SPECIAL_MESSAGE: 'Арнайы жауап жіберу',
};

export const AUDIT_ACTIONS = {
  'auth.login': 'Жүйеге кірді',
  'auth.logout': 'Жүйеден шықты',
  'auth.login_failed': 'Кіру сәтсіз',
  'auth.password_changed': 'Құпия сөз өзгертілді',
  'admin.created': 'Әкімші қосылды',
  'admin.updated': 'Әкімші жаңартылды',
  'admin.deleted': 'Әкімші жойылды',
  'campaign.created': 'Кампания құрылды',
  'campaign.updated': 'Кампания жаңартылды',
  'campaign.activated': 'Кампания іске қосылды',
  'campaign.paused': 'Кампания кідіртілді',
  'campaign.archived': 'Кампания мұрағатталды',
  'campaign.deleted': 'Кампания жойылды',
  'campaign.duplicated': 'Кампания көшірілді',
  'campaign.event_time_changed': 'Іс-шара уақыты өзгертілді',
  'step.created': 'Қадам қосылды',
  'step.updated': 'Қадам жаңартылды',
  'step.deleted': 'Қадам жойылды',
  'step.reordered': 'Қадамдар реті өзгертілді',
  'trigger.created': 'Триггер қосылды',
  'trigger.updated': 'Триггер жаңартылды',
  'trigger.deleted': 'Триггер жойылды',
  'template.created': 'Шаблон құрылды',
  'template.updated': 'Шаблон жаңартылды',
  'template.deleted': 'Шаблон жойылды',
  'template.duplicated': 'Шаблон көшірілді',
  'contact.updated': 'Байланыс жаңартылды',
  'contact.blocked': 'Байланыс бұғатталды',
  'contact.unblocked': 'Бұғаттау алынды',
  'contact.unsubscribed': 'Жазылымнан шығарылды',
  'contact.resubscribed': 'Жазылым қалпына келді',
  'contact.deleted': 'Байланыс жойылды',
  'message.manual_sent': 'Қолмен хабарлама',
  'contacts.bulk_action': 'Топтық әрекет',
  'contacts.exported': 'Экспорт',
  'media.uploaded': 'Файл жүктелді',
  'media.deleted': 'Файл жойылды',
  'job.requeued': 'Қайта кезекке қойылды',
  'job.cancelled': 'Хабарлама тоқтатылды',
};

export function messageTypeLabel(type) {
  const labels = {
    TEXT: 'Мәтін', IMAGE: 'Сурет', VIDEO: 'Бейне', AUDIO: 'Аудио',
    VOICE: 'Дауыстық', DOCUMENT: 'Құжат', STICKER: 'Стикер',
    LOCATION: 'Геолокация', CONTACT: 'Контакт', POLL: 'Сауалнама',
    REACTION: 'Реакция', UNKNOWN: 'Белгісіз',
  };
  return labels[type] || type;
}

export function initials(name, phone) {
  const source = (name || '').trim();
  if (source) {
    const words = source.split(/\s+/).slice(0, 2);
    return words.map((w) => w[0].toUpperCase()).join('');
  }
  const digits = String(phone || '').replace(/\D/g, '');
  return digits.slice(-2) || '?';
}
