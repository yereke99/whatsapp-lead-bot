import { api } from '../api.js';
import {
  el, mount, clear, icon, button, badge, notify, emptyState,
  openModal, field, input, textarea, select, checkbox, confirmDialog,
} from '../ui.js';
import {
  formatDateTime, formatNumber, dateInputValue, timeInputValue,
  CAMPAIGN_STATUS, EXISTING_BEHAVIOR, RESUME_POLICY, timezone,
} from '../format.js';

const TIMEZONES = [
  'Asia/Almaty', 'Asia/Aqtobe', 'Asia/Atyrau', 'Asia/Oral', 'Asia/Qostanay',
  'Asia/Tashkent', 'Asia/Bishkek', 'Europe/Moscow', 'UTC',
];

export async function renderCampaigns(root, { navigate }) {
  const grid = el('div', { class: 'stack' });

  mount(root,
    el('div', { class: 'page-head' },
      el('div', { class: 'page-head__text' },
        el('h1', {}, 'Кампаниялар'),
        el('p', { class: 'page-head__desc' },
          'Әр кампанияның өз триггері, уақыты және автоматтандыру тізбегі бар')),
      el('div', { class: 'page-head__actions' },
        button('Жаңа кампания', { iconName: 'plus', variant: 'primary', onClick: () => openCampaignForm(null, load) }))),
    grid,
  );

  await load();

  async function load() {
    mount(grid, el('div', { class: 'card' }, el('div', { class: 'card__body' },
      el('div', { class: 'skeleton', style: { width: '45%', height: '20px' } }),
      el('div', { class: 'skeleton mt-3', style: { width: '75%' } }))));

    try {
      const campaigns = await api.campaigns({ include_archived: 'true' });
      clear(grid);

      if (!campaigns || campaigns.length === 0) {
        grid.append(el('div', { class: 'card' }, el('div', { class: 'card__body' },
          emptyState('Кампания жоқ',
            'Кампания құрып, триггер сөзін және вебинар уақытын белгілеңіз.',
            button('Жаңа кампания', { variant: 'primary', iconName: 'plus', onClick: () => openCampaignForm(null, load) })))));
        return;
      }

      for (const campaign of campaigns) {
        grid.append(campaignCard(campaign));
      }
    } catch (err) {
      mount(grid, el('div', { class: 'alert alert--danger' }, err.message));
    }
  }

  function campaignCard(campaign) {
    const status = CAMPAIGN_STATUS[campaign.status] || { label: campaign.status, tone: 'neutral' };
    const triggers = campaign.triggers || [];

    const actions = el('div', { class: 'row' });

    if (campaign.status === 'ACTIVE') {
      actions.append(button('Кідірту', { size: 'sm', iconName: 'pause', onClick: () => setStatus(campaign, 'PAUSED') }));
    } else if (campaign.status === 'DRAFT' || campaign.status === 'PAUSED') {
      actions.append(button('Іске қосу', { size: 'sm', variant: 'primary', iconName: 'play', onClick: () => setStatus(campaign, 'ACTIVE') }));
    }

    actions.append(
      button('Ашу', { size: 'sm', iconName: 'eye', onClick: () => navigate(`/campaigns/${campaign.id}`) }),
      button('', { size: 'sm', iconName: 'edit', title: 'Өңдеу', onClick: () => openCampaignForm(campaign, load) }),
      button('', { size: 'sm', iconName: 'copy', title: 'Көшіру', onClick: () => duplicate(campaign) }),
    );

    if (campaign.status !== 'ARCHIVED') {
      actions.append(button('', { size: 'sm', iconName: 'trash', title: 'Мұрағаттау', onClick: () => archive(campaign) }));
    }

    return el('div', { class: 'card' },
      el('div', { class: 'card__head' },
        el('div', { style: { flex: '1', minWidth: '0' } },
          el('div', { class: 'row' },
            el('span', { class: 'card__title' }, campaign.name),
            badge(status.label, status.tone)),
          el('div', { class: 'card__sub' }, campaignSchedule(campaign))),
        el('div', { class: 'card__actions' }, actions)),
      el('div', { class: 'card__body' },
        campaign.description ? el('p', { class: 'muted small mb-3' }, campaign.description) : null,
        el('div', { class: 'grid grid--stats' },
          miniStat('Клиенттер', formatNumber(campaign.contact_count)),
          miniStat('Жіберілді', formatNumber(campaign.sent_count)),
          miniStat('Кезекте', formatNumber(campaign.pending_jobs)),
          miniStat('Қадамдар', formatNumber((campaign.steps || []).length || '—'))),
        el('div', { class: 'mt-3' },
          el('div', { class: 'label' }, 'Триггерлер'),
          triggers.length === 0
            ? el('p', { class: 'muted small' }, 'Триггер қосылмаған — кампания іске қосылмайды')
            : el('div', { class: 'chips' },
                ...triggers.map((trigger) => badge(
                  trigger.keyword,
                  trigger.is_active ? 'success' : 'neutral')))),
        campaign.webinar_link
          ? el('div', { class: 'mt-3 row small' },
              icon('link', 14),
              el('a', { href: campaign.webinar_link, target: '_blank', rel: 'noopener noreferrer' },
                campaign.webinar_link))
          : null),
    );
  }

  async function setStatus(campaign, status) {
    try {
      await api.setCampaignStatus(campaign.id, status);
      notify.success(status === 'ACTIVE' ? 'Кампания іске қосылды' : 'Кампания кідіртілді');
      await load();
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function duplicate(campaign) {
    try {
      const copy = await api.duplicateCampaign(campaign.id);
      notify.success(`Көшірме құрылды: ${copy.name}`);
      await load();
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function archive(campaign) {
    const confirmed = await confirmDialog({
      title: 'Кампанияны мұрағаттау',
      message: 'Триггерлер өшіріледі және барлық жоспарланған хабарламалар тоқтатылады.',
      confirmLabel: 'Мұрағаттау',
      danger: true,
    });
    if (!confirmed) return;

    try {
      await api.setCampaignStatus(campaign.id, 'ARCHIVED');
      notify.success('Мұрағатталды');
      await load();
    } catch (err) {
      notify.error(err.message);
    }
  }
}

// campaignSchedule is the one-line answer to "when does this campaign fire?".
// A recurring series reports the webinar that is coming rather than the day it
// started, which is the only date the operator can act on.
function campaignSchedule(campaign) {
  if (campaign.is_daily_recurring) {
    const next = campaign.next_occurrence_at
      ? ` · келесі: ${formatDateTime(campaign.next_occurrence_at)}`
      : '';
    return `Күн сайын ${campaign.recurrence_time || ''} · ${campaign.timezone}${next}`;
  }
  return campaign.event_start_at
    ? `${formatDateTime(campaign.event_start_at)} · ${campaign.timezone}`
    : 'Іс-шара уақыты белгіленбеген';
}

function miniStat(label, value) {
  return el('div', { class: 'stat' },
    el('div', { class: 'stat__label' }, label),
    el('div', { class: 'stat__value tnum', style: { fontSize: '20px' } }, value));
}

// openCampaignForm handles both create and edit.
export async function openCampaignForm(campaign, onSaved) {
  const isEdit = Boolean(campaign);

  let templates = [];
  try {
    templates = await api.templates();
  } catch {
    templates = [];
  }

  const nameInput = input({ value: campaign?.name || '', placeholder: 'Мысалы: Түрік айраны вебинары', required: true });
  const descInput = textarea({ placeholder: 'Кампанияның қысқаша сипаттамасы' });
  descInput.value = campaign?.description || '';

  const dateInput = input({ type: 'date', value: campaign?.event_start_at ? dateInputValue(campaign.event_start_at) : '' });
  const timeInput = input({ type: 'time', value: campaign?.event_start_at ? timeInputValue(campaign.event_start_at) : '21:00' });

  // Daily recurring webinar. One toggle and one time: everything else — the
  // steps, their offsets, the link, the audience rules — is reused as it
  // stands, and each day's webinar simply becomes the anchor they measure
  // from. The date field above keeps working and becomes the day the series
  // starts.
  const recurringTime = input({ type: 'time', value: campaign?.recurrence_time || '' });
  const recurringDaily = checkbox('Вебинар күн сайын қайталансын', {
    checked: campaign?.is_daily_recurring ?? false,
    hint: 'Қосылса, вебинар күнделікті сол уақытта өтеді. Барлық хабарламалар сол күнгі вебинарға '
      + 'қарай автоматты есептеледі — кампанияны күн сайын көшірудің қажеті жоқ.',
  });

  const recurringField = field('Күнделікті вебинар уақыты', recurringTime, {
    hint: 'Жоғарыдағы «Іс-шара күні» — қайталанудың басталатын күні.',
  });
  const recurringNote = el('p', { class: 'muted small' });

  const syncRecurring = () => {
    const on = recurringDaily.input.checked;
    recurringField.classList.toggle('hidden', !on);
    recurringNote.classList.toggle('hidden', !on);
    if (on && !recurringTime.value) recurringTime.value = timeInput.value || '21:00';
    recurringNote.textContent = on && campaign?.next_occurrence_at
      ? `Келесі вебинар: ${formatDateTime(campaign.next_occurrence_at)} (${campaign.timezone})`
      : '';
  };
  recurringDaily.input.addEventListener('change', syncRecurring);
  syncRecurring();

  const tzSelect = select(
    TIMEZONES.map((tz) => ({ value: tz, label: tz })),
    { value: campaign?.timezone || timezone() },
  );

  const linkInput = input({ type: 'url', value: campaign?.webinar_link || '', placeholder: 'https://…' });

  const behaviorSelect = select(
    Object.entries(EXISTING_BEHAVIOR).map(([value, label]) => ({ value, label })),
    { value: campaign?.existing_contact_behavior || 'IGNORE' },
  );

  const replyTemplateSelect = select(
    [{ value: '', label: '— таңдалмаған —' },
      ...templates.map((t) => ({ value: t.id, label: t.name }))],
    { value: campaign?.existing_contact_template_id || '' },
  );

  const replyField = field('Қайталанған триггерге жауап', replyTemplateSelect, {
    hint: 'Тек «Арнайы жауап жіберу» режимінде қолданылады',
  });
  const syncReplyVisibility = () => {
    replyField.classList.toggle('hidden', behaviorSelect.value !== 'SPECIAL_MESSAGE');
  };
  behaviorSelect.addEventListener('change', syncReplyVisibility);
  syncReplyVisibility();

  const keywordsInput = input({
    value: (campaign?.unsubscribe_keywords || ['STOP', 'ТОҚТАТУ', 'СТОП']).join(', '),
    placeholder: 'STOP, ТОҚТАТУ',
  });

  const catchUp = checkbox('Кешіккен қадамдарды жіберу', {
    checked: campaign?.catch_up_missed_steps ?? true,
    hint: 'Кеш қосылған клиентке ең соңғы өтіп кеткен хабарлама бірден жіберіледі',
  });

  const attemptsInput = input({
    type: 'number', min: '1', max: '20',
    value: String(campaign?.max_send_attempts || 5),
  });

  // What to do with messages whose moment passed while the campaign was off.
  const resumeSelect = select(
    Object.entries(RESUME_POLICY).map(([value, label]) => ({ value, label })),
    { value: campaign?.resume_policy || 'SKIP_EXPIRED' },
  );

  const pinVersion = checkbox('Кезектегі хабарламалар шаблонның ағымдағы нұсқасында қалсын', {
    checked: campaign?.pin_template_version ?? false,
    hint: 'Өшірулі болса, шаблонды өңдегенде әлі жіберілмеген хабарламалар жаңа нұсқамен кетеді',
  });

  // Optional safety caps. Empty means no limit.
  const perHourInput = input({
    type: 'number', min: '1', placeholder: 'шектеусіз',
    value: campaign?.max_messages_per_hour ? String(campaign.max_messages_per_hour) : '',
  });
  const perDayInput = input({
    type: 'number', min: '1', placeholder: 'шектеусіз',
    value: campaign?.max_messages_per_day ? String(campaign.max_messages_per_day) : '',
  });
  const maxContactsInput = input({
    type: 'number', min: '1', placeholder: 'шектеусіз',
    value: campaign?.max_active_contacts ? String(campaign.max_active_contacts) : '',
  });

  const errorBox = el('div', { class: 'alert alert--danger hidden' });

  const body = el('div', {},
    errorBox,
    field('Атауы', nameInput),
    field('Сипаттама', descInput),
    el('div', { class: 'form-grid form-grid--3' },
      field('Іс-шара күні', dateInput),
      field('Уақыты', timeInput),
      field('Уақыт белдеуі', tzSelect)),
    el('div', { class: 'field' }, recurringDaily),
    recurringField,
    recurringNote,
    field('Вебинар сілтемесі', linkInput, {
      hint: 'Шаблондарда {{webinar_link}} арқылы қолданылады',
    }),
    el('div', { class: 'form-grid' },
      field('Қайталанған триггер', behaviorSelect),
      field('Максимал әрекет саны', attemptsInput, { hint: 'Қате болғанда қайталау шегі' })),
    replyField,
    field('Жазылымнан шығу сөздері', keywordsInput, {
      hint: 'Үтір арқылы бөліңіз. Клиент осы сөздердің бірін жазса, автоматтандыру тоқтайды.',
    }),
    el('div', { class: 'field' }, catchUp),
    field('Тоқтатудан кейін жалғастыру', resumeSelect, {
      hint: 'Кампания тоқтап тұрғанда уақыты өтіп кеткен хабарламалармен не істеу керек',
    }),
    el('div', { class: 'field' }, pinVersion),
    el('div', { class: 'label mt-3' }, 'Қауіпсіздік шектеулері'),
    el('div', { class: 'form-grid form-grid--3' },
      field('Сағатына', perHourInput),
      field('Тәулігіне', perDayInput),
      field('Белсенді клиент', maxContactsInput)),
    el('p', { class: 'muted small' },
      'Бос қалдырсаңыз — шектеу жоқ. Шекке жеткенде кезек тоқтап, әкімші панелінде ескерту көрсетіледі; '
      + 'хабарламалар жоғалмайды.'),
  );

  const saveButton = button(isEdit ? 'Сақтау' : 'Құру', { variant: 'primary' });

  const handle = openModal({
    title: isEdit ? 'Кампанияны өңдеу' : 'Жаңа кампания',
    wide: true,
    body,
    footer: [button('Болдырмау', { onClick: () => handle.close() }), saveButton],
  });

  saveButton.addEventListener('click', async () => {
    errorBox.classList.add('hidden');

    const payload = {
      name: nameInput.value.trim(),
      description: descInput.value.trim(),
      event_type: 'WEBINAR',
      event_date: dateInput.value,
      event_time: timeInput.value,
      timezone: tzSelect.value,
      is_daily_recurring: recurringDaily.input.checked,
      // The backend falls back to the event time and date when these are
      // blank, so a bare tick of the box means "every day, at the hour I have
      // already chosen".
      recurrence_time: recurringDaily.input.checked ? recurringTime.value : '',
      recurrence_start_date: recurringDaily.input.checked ? dateInput.value : '',
      webinar_link: linkInput.value.trim(),
      existing_contact_behavior: behaviorSelect.value,
      existing_contact_template_id: replyTemplateSelect.value || '',
      unsubscribe_keywords: keywordsInput.value.split(',').map((k) => k.trim()).filter(Boolean),
      catch_up_missed_steps: catchUp.input.checked,
      max_send_attempts: Number(attemptsInput.value) || 5,
      resume_policy: resumeSelect.value,
      pin_template_version: pinVersion.input.checked,
      max_messages_per_hour: optionalLimit(perHourInput.value),
      max_messages_per_day: optionalLimit(perDayInput.value),
      max_active_contacts: optionalLimit(maxContactsInput.value),
    };

    if (!payload.name) {
      showError('Кампания атауын енгізіңіз');
      return;
    }
    if (payload.is_daily_recurring && !payload.recurrence_time) {
      showError('Күнделікті вебинар уақытын көрсетіңіз');
      return;
    }

    saveButton.disabled = true;
    try {
      if (isEdit) {
        const result = await api.updateCampaign(campaign.id, payload);
        if (result.event_time_changed) {
          notify.success(`Уақыт өзгертілді. ${result.rescheduled_jobs} жоспарланған хабарлама қайта есептелді.`);
        } else {
          notify.success('Сақталды');
        }
      } else {
        await api.createCampaign(payload);
        notify.success('Кампания құрылды');
      }
      handle.close();
      if (onSaved) await onSaved();
    } catch (err) {
      showError(err.message);
    } finally {
      saveButton.disabled = false;
    }
  });

  function showError(message) {
    errorBox.textContent = message;
    errorBox.classList.remove('hidden');
  }
}

// optionalLimit turns an empty safety-limit field into "no limit" rather than
// zero, which would mean "allow nothing".
function optionalLimit(value) {
  const trimmed = String(value ?? '').trim();
  if (trimmed === '') return null;
  const parsed = Number(trimmed);
  return Number.isFinite(parsed) && parsed > 0 ? Math.floor(parsed) : null;
}
