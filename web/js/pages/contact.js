import { api } from '../api.js';
import {
  el, mount, clear, icon, button, badge, card, avatar, notify,
  emptyState, linkify, confirmDialog, field, input, textarea, openModal,
} from '../ui.js';
import {
  formatPhone, formatDateTime, formatTime, dayLabel, dateInputValue,
  formatNumber, messageTypeLabel, offsetLabel,
  CONTACT_STATUS, JOB_STATUS, MESSAGE_STATUS,
} from '../format.js';
import { on } from '../store.js';

export async function renderContactDetail(root, { params, navigate }) {
  const contactId = params[0];

  mount(root, el('div', { class: 'card' }, el('div', { class: 'card__body' },
    el('div', { class: 'skeleton', style: { width: '40%', height: '22px' } }),
    el('div', { class: 'skeleton mt-3', style: { width: '70%' } }))));

  const [detail, messages] = await Promise.all([
    api.contact(contactId),
    api.contactMessages(contactId, { limit: 200 }),
  ]);

  let contact = detail.contact;
  const enrollments = detail.enrollments || [];
  const scheduled = detail.scheduled || [];

  const status = CONTACT_STATUS[contact.status] || { label: contact.status, tone: 'neutral' };

  const header = el('div', { class: 'page-head' },
    el('div', { class: 'row', style: { flex: '1', minWidth: '260px' } },
      button('', { iconName: 'back', variant: 'ghost', title: 'Артқа', onClick: () => navigate('/contacts') }),
      avatar(contact, 'lg'),
      el('div', { class: 'page-head__text' },
        el('h1', {}, contact.name || contact.push_name || formatPhone(contact.phone)),
        el('div', { class: 'row small mt-2' },
          el('span', { class: 'muted' }, formatPhone(contact.phone)),
          badge(status.label, status.tone),
          contact.opted_out ? badge('Жазылымнан шықты', 'warn') : null,
          contact.blocked_at ? badge('Бұғатталған', 'danger') : null))),
    el('div', { class: 'page-head__actions' },
      button('Өңдеу', { iconName: 'edit', onClick: openEditor }),
      button(contact.opted_out ? 'Жазылымды қалпына келтіру' : 'Жазылымнан шығару', {
        onClick: toggleSubscription,
      }),
      button(contact.blocked_at ? 'Бұғаттауды алу' : 'Бұғаттау', {
        variant: contact.blocked_at ? '' : 'danger',
        onClick: toggleBlock,
      })),
  );

  const infoCard = card('Ақпарат', {
    body: el('dl', { class: 'kv' },
      el('dt', {}, 'Телефон'), el('dd', {}, formatPhone(contact.phone)),
      el('dt', {}, 'WhatsApp ID'), el('dd', { class: 'mono' }, contact.chat_id),
      el('dt', {}, 'Дереккөз'), el('dd', {}, contact.source || '—'),
      el('dt', {}, 'Кампания'), el('dd', {}, contact.campaign_name || '—'),
      el('dt', {}, 'Триггер'), el('dd', {}, contact.first_trigger_keyword || '—'),
      el('dt', {}, 'Алғашқы байланыс'), el('dd', {}, formatDateTime(contact.first_contact_at)),
      el('dt', {}, 'Соңғы белсенділік'), el('dd', {}, formatDateTime(contact.last_activity_at)),
      el('dt', {}, 'Кіріс / шығыс'), el('dd', { class: 'tnum' },
        `${formatNumber(contact.incoming_count)} / ${formatNumber(contact.outgoing_count)}`),
      el('dt', {}, 'Тіркелген'), el('dd', {}, formatDateTime(contact.created_at)),
      contact.notes ? el('dt', {}, 'Ескертпе') : null,
      contact.notes ? el('dd', { style: { whiteSpace: 'pre-wrap' } }, contact.notes) : null),
  });

  const enrollmentCard = card('Кампаниядағы жағдайы', {
    flush: true,
    body: enrollments.length === 0
      ? el('div', { class: 'card__body' }, el('p', { class: 'muted small' }, 'Кампанияға қосылмаған'))
      : el('div', { class: 'table-wrap' },
          el('table', { class: 'table table--cards' },
            el('thead', {}, el('tr', {},
              el('th', {}, 'Кампания'),
              el('th', {}, 'Мәртебе'),
              el('th', {}, 'Триггер'),
              el('th', { class: 'is-numeric' }, 'Жіберілді'),
              el('th', { class: 'is-numeric' }, 'Кезекте'),
              el('th', {}, 'Қосылған'))),
            el('tbody', {}, ...enrollments.map((enrollment) => el('tr', {},
              el('td', { 'data-label': 'Кампания' },
                el('a', { href: `/campaigns/${enrollment.campaign_id}`, 'data-link': '' }, enrollment.campaign_name)),
              el('td', { 'data-label': 'Мәртебе' }, badge(
                { ACTIVE: 'Белсенді', COMPLETED: 'Аяқталды', CANCELLED: 'Тоқтатылды', UNSUBSCRIBED: 'Шықты' }[enrollment.status] || enrollment.status,
                { ACTIVE: 'success', COMPLETED: 'info', CANCELLED: 'neutral', UNSUBSCRIBED: 'warn' }[enrollment.status] || 'neutral')),
              el('td', { 'data-label': 'Триггер', class: 'small' }, enrollment.trigger_keyword || '—'),
              el('td', { 'data-label': 'Жіберілді', class: 'is-numeric tnum' }, formatNumber(enrollment.sent_jobs)),
              el('td', { 'data-label': 'Кезекте', class: 'is-numeric tnum' }, formatNumber(enrollment.pending_jobs)),
              el('td', { 'data-label': 'Қосылған' }, formatDateTime(enrollment.enrolled_at))))))),
  });

  const scheduleCard = card('Жоспарланған хабарламалар', {
    subtitle: `${scheduled.length} жазба`,
    flush: true,
    body: scheduled.length === 0
      ? el('div', { class: 'card__body' }, el('p', { class: 'muted small' }, 'Жоспарланған хабарлама жоқ'))
      : el('div', { class: 'table-wrap' },
          el('table', { class: 'table table--cards' },
            el('thead', {}, el('tr', {},
              el('th', {}, 'Уақыты'),
              el('th', {}, 'Қадам'),
              el('th', {}, 'Мәртебе'),
              el('th', {}, 'Мәлімет'))),
            el('tbody', {}, ...scheduled.map((job) => {
              const jobStatus = JOB_STATUS[job.status] || { label: job.status, tone: 'neutral' };
              return el('tr', {},
                el('td', { 'data-label': 'Уақыты', class: 'nowrap' }, formatDateTime(job.scheduled_at)),
                el('td', { 'data-label': 'Қадам' }, job.step_name || '—'),
                el('td', { 'data-label': 'Мәртебе' }, badge(jobStatus.label, jobStatus.tone)),
                el('td', { 'data-label': 'Мәлімет', class: 'small muted' },
                  job.last_error || job.cancel_reason || (job.attempt_count > 0 ? `${job.attempt_count} әрекет` : '—')));
            })))),
  });

  const thread = el('div', {
    class: 'chat-thread',
    style: { maxHeight: '560px', background: 'var(--surface-sunken)', borderRadius: '0 0 var(--radius-lg) var(--radius-lg)' },
  });

  renderThread();

  const conversationCard = card('Хабарламалар тарихы', {
    subtitle: `${messages.length} хабарлама`,
    flush: true,
    body: messages.length === 0
      ? el('div', { class: 'card__body' }, emptyState('Хабарлама жоқ', 'Бұл байланыспен әлі сөйлесу болмаған.'))
      : thread,
  });

  mount(root,
    header,
    el('div', { class: 'grid grid--2' }, infoCard, enrollmentCard),
    el('div', { class: 'mt-3' }, conversationCard),
    el('div', { class: 'mt-3' }, scheduleCard),
  );

  function renderThread() {
    clear(thread);

    let lastDay = '';
    for (const message of messages) {
      const day = dateInputValue(message.created_at);
      if (day !== lastDay) {
        lastDay = day;
        thread.append(el('div', { class: 'chat-day' }, dayLabel(message.created_at)));
      }
      thread.append(bubble(message));
    }
  }

  function bubble(message) {
    const outgoing = message.direction === 'OUTGOING';
    const messageStatus = MESSAGE_STATUS[message.status] || { label: message.status, tone: 'neutral' };

    const node = el('div', {
      class: `bubble bubble--${outgoing ? 'out' : 'in'}${message.status === 'FAILED' ? ' is-failed' : ''}`,
    });

    if (outgoing) {
      node.append(el('div', { class: 'bubble__source' },
        message.is_manual
          ? `Оператор${message.admin_name ? ` · ${message.admin_name}` : ''}`
          : message.step_name
            ? `Автоматтандыру · ${message.step_name}`
            : 'Автоматтандыру'));
    }

    if (message.media_access_url) {
      if (message.type === 'IMAGE' || message.type === 'STICKER') {
        node.append(el('div', { class: 'bubble__media' },
          el('img', { src: message.media_access_url, alt: '', loading: 'lazy' })));
      } else if (message.type === 'VIDEO') {
        node.append(el('div', { class: 'bubble__media' },
          el('video', { src: message.media_access_url, controls: true, preload: 'metadata' })));
      } else if (message.type === 'VOICE' || message.type === 'AUDIO') {
        node.append(el('div', { class: 'bubble__media' },
          el('audio', { src: message.media_access_url, controls: true, preload: 'metadata' })));
      } else {
        node.append(el('a', { class: 'bubble__file', href: `${message.media_access_url}?download=true` },
          icon('file', 18),
          el('div', {}, el('div', { class: 'bubble__file-name' }, message.file_name || 'Файл'))));
      }
    }

    if (message.text) {
      node.append(el('div', { class: 'bubble__text' }, linkify(message.text)));
    } else if (!message.media_access_url) {
      node.append(el('div', { class: 'bubble__text muted' }, `[${messageTypeLabel(message.type)}]`));
    }

    node.append(el('div', { class: 'bubble__meta' },
      el('span', {}, formatTime(message.created_at)),
      outgoing ? el('span', { class: `badge badge--${messageStatus.tone}`, style: { padding: '0 6px', fontSize: '10px' } }, messageStatus.label) : null));

    if (message.error) {
      node.append(el('div', { class: 'bubble__error' }, message.error));
    }
    return node;
  }

  function openEditor() {
    const nameInput = input({ value: contact.name || '', placeholder: 'Клиенттің аты' });
    const notesInput = textarea({ placeholder: 'Ішкі ескертпе' });
    notesInput.value = contact.notes || '';

    const handle = openModal({
      title: 'Байланысты өңдеу',
      body: el('div', {},
        field('Аты', nameInput, { hint: 'Бұл ат {{first_name}} айнымалысында қолданылады' }),
        field('Ескертпе', notesInput, { hint: 'Тек әкімшілерге көрінеді' })),
      footer: [
        button('Болдырмау', { onClick: () => handle.close() }),
        button('Сақтау', {
          variant: 'primary',
          onClick: async () => {
            try {
              contact = await api.updateContact(contactId, {
                name: nameInput.value.trim(),
                notes: notesInput.value,
              });
              notify.success('Сақталды');
              handle.close();
              renderContactDetail(root, { params, navigate });
            } catch (err) {
              notify.error(err.message);
            }
          },
        }),
      ],
    });
  }

  async function toggleSubscription() {
    const optOut = !contact.opted_out;
    const confirmed = await confirmDialog({
      title: optOut ? 'Жазылымнан шығару' : 'Жазылымды қалпына келтіру',
      message: optOut
        ? 'Барлық жоспарланған хабарламалар тоқтатылады. Жалғастыру керек пе?'
        : 'Клиент қайтадан кампанияларға қатыса алады. Жалғастыру керек пе?',
      confirmLabel: optOut ? 'Шығару' : 'Қалпына келтіру',
      danger: optOut,
    });
    if (!confirmed) return;

    try {
      await api.unsubscribeContact(contactId, optOut);
      notify.success(optOut ? 'Жазылымнан шығарылды' : 'Жазылым қалпына келтірілді');
      renderContactDetail(root, { params, navigate });
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function toggleBlock() {
    const blocked = !contact.blocked_at;
    const confirmed = await confirmDialog({
      title: blocked ? 'Бұғаттау' : 'Бұғаттауды алу',
      message: blocked
        ? 'Бұғатталған клиентке ешқандай хабарлама жіберілмейді.'
        : 'Клиентке қайта хабарлама жіберуге болады.',
      confirmLabel: blocked ? 'Бұғаттау' : 'Бұғаттауды алу',
      danger: blocked,
    });
    if (!confirmed) return;

    try {
      await api.blockContact(contactId, blocked);
      notify.success(blocked ? 'Бұғатталды' : 'Бұғаттау алынды');
      renderContactDetail(root, { params, navigate });
    } catch (err) {
      notify.error(err.message);
    }
  }

  // Keep the timeline live while the operator is looking at it.
  const off = on('message.created', (event) => {
    if (event.contact_id !== contactId) return;
    messages.push(event.data);
    renderThread();
    thread.scrollTop = thread.scrollHeight;
  });

  return off;
}
