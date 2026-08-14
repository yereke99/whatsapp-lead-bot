// Campaign detail: the automation builder, trigger management and the
// pre-activation timeline preview.

import { api } from '../api.js';
import {
  el, mount, clear, icon, button, badge, card, notify, emptyState,
  openModal, field, input, select, checkbox, confirmDialog, linkify,
} from '../ui.js';
import {
  formatDateTime, formatNumber, offsetLabel, splitOffset, buildOffsetSeconds,
  CAMPAIGN_STATUS, TEMPLATE_TYPE, MATCH_MODE,
} from '../format.js';
import { openCampaignForm } from './campaigns.js';

export async function renderCampaignDetail(root, { params, navigate }) {
  const campaignId = params[0];

  let campaign = null;
  let templates = [];

  await reload();

  async function reload() {
    [campaign, templates] = await Promise.all([
      api.campaign(campaignId),
      api.templates().catch(() => []),
    ]);
    render();
  }

  function render() {
    const status = CAMPAIGN_STATUS[campaign.status] || { label: campaign.status, tone: 'neutral' };

    const header = el('div', { class: 'page-head' },
      el('div', { class: 'row', style: { flex: '1', minWidth: '260px' } },
        button('', { iconName: 'back', variant: 'ghost', title: 'Артқа', onClick: () => navigate('/campaigns') }),
        el('div', { class: 'page-head__text' },
          el('div', { class: 'row' },
            el('h1', {}, campaign.name),
            badge(status.label, status.tone)),
          el('p', { class: 'page-head__desc' },
            campaign.event_start_at
              ? `Іс-шара: ${formatDateTime(campaign.event_start_at)} (${campaign.timezone})`
              : 'Іс-шара уақыты белгіленбеген'))),
      el('div', { class: 'page-head__actions' },
        campaign.status === 'ACTIVE'
          ? button('Кідірту', { iconName: 'pause', onClick: () => setStatus('PAUSED') })
          : button('Іске қосу', { iconName: 'play', variant: 'primary', onClick: () => setStatus('ACTIVE') }),
        button('Баптаулар', { iconName: 'settings', onClick: () => openCampaignForm(campaign, reload) })),
    );

    const stats = el('div', { class: 'grid grid--stats' },
      statTile('Клиенттер', formatNumber(campaign.contact_count)),
      statTile('Жіберілді', formatNumber(campaign.sent_count)),
      statTile('Кезекте', formatNumber(campaign.pending_jobs)),
      statTile('Қадамдар', formatNumber((campaign.steps || []).filter((s) => s.enabled).length)),
    );

    const warnings = [];
    if (!campaign.event_start_at) {
      warnings.push('Іс-шара күні мен уақыты белгіленбеген — кампанияны іске қосу мүмкін емес.');
    }
    if ((campaign.triggers || []).length === 0) {
      warnings.push('Триггер қосылмаған — клиенттер воронкаға кіре алмайды.');
    }
    if ((campaign.steps || []).filter((s) => s.enabled).length === 0) {
      warnings.push('Қосулы қадам жоқ — ешқандай хабарлама жіберілмейді.');
    }
    if (!campaign.webinar_link && (campaign.steps || []).some((s) => (s.template_preview || '').includes('{{webinar_link}}'))) {
      warnings.push('Шаблондарда {{webinar_link}} қолданылған, бірақ кампанияда сілтеме енгізілмеген.');
    }

    const warningBlock = warnings.length === 0 ? null : el('div', { class: 'alert alert--warn' },
      icon('alert', 17),
      el('div', {},
        el('div', { class: 'alert__title' }, 'Іске қосу алдында тексеріңіз'),
        el('ul', { style: { margin: '4px 0 0', paddingLeft: '18px' } },
          ...warnings.map((text) => el('li', {}, text)))));

    mount(root,
      header,
      warningBlock,
      stats,
      el('div', { class: 'mt-3' }, triggersCard()),
      el('div', { class: 'mt-3' }, automationCard()),
    );
  }

  function statTile(label, value) {
    return el('div', { class: 'stat' },
      el('div', { class: 'stat__label' }, label),
      el('div', { class: 'stat__value tnum', style: { fontSize: '21px' } }, value));
  }

  // -------------------------------------------------------------- triggers --

  function triggersCard() {
    const triggers = campaign.triggers || [];

    const body = triggers.length === 0
      ? emptyState('Триггер жоқ',
          'Клиент воронкаға кіру үшін жіберетін нақты мәтінді қосыңыз.',
          button('Триггер қосу', { variant: 'primary', iconName: 'plus', onClick: () => openTriggerForm(null) }))
      : el('div', { class: 'stack stack--sm' },
          ...triggers.map((trigger) => el('div', {
            class: 'row row--between',
            style: {
              padding: '10px 12px', border: '1px solid var(--border)',
              borderRadius: 'var(--radius)', background: 'var(--surface-muted)',
            },
          },
            el('div', { style: { minWidth: '0', flex: '1' } },
              el('div', { class: 'mono', style: { wordBreak: 'break-word' } }, trigger.keyword),
              el('div', { class: 'row small subtle mt-2' },
                badge(MATCH_MODE[trigger.match_mode] || trigger.match_mode, 'neutral'),
                trigger.is_active ? badge('Белсенді', 'success') : badge('Өшірулі', 'neutral'))),
            el('div', { class: 'row' },
              button('', { iconName: 'edit', size: 'sm', title: 'Өңдеу', onClick: () => openTriggerForm(trigger) }),
              button('', { iconName: 'trash', size: 'sm', title: 'Жою', onClick: () => deleteTrigger(trigger) })))));

    return card('Триггерлер', {
      subtitle: 'Осы мәтінді жіберген клиент автоматты түрде кампанияға қосылады',
      actions: triggers.length > 0
        ? button('Қосу', { size: 'sm', iconName: 'plus', onClick: () => openTriggerForm(null) })
        : null,
      body,
    });
  }

  function openTriggerForm(trigger) {
    const keywordInput = el('textarea', {
      class: 'textarea',
      rows: 3,
      placeholder: 'Айран/Қаймақ кәсібі бойынша тегін сабаққа қатысқым келеді',
    });
    keywordInput.value = trigger?.keyword || '';

    const modeSelect = select(
      Object.entries(MATCH_MODE).map(([value, label]) => ({ value, label })),
      { value: trigger?.match_mode || 'EXACT' },
    );

    const activeBox = checkbox('Белсенді', { checked: trigger?.is_active ?? true });

    const handle = openModal({
      title: trigger ? 'Триггерді өңдеу' : 'Жаңа триггер',
      body: el('div', {},
        field('Триггер мәтіні', keywordInput, {
          hint: 'Регистр, артық бос орындар мен Unicode айырмашылықтары автоматты түрде ескерілмейді',
        }),
        field('Сәйкестендіру режимі', modeSelect, {
          hint: '«Дәл сәйкестік» — ең қауіпсіз нұсқа: кездейсоқ хабарламалар воронканы іске қоспайды',
        }),
        el('div', { class: 'field' }, activeBox)),
      footer: [
        button('Болдырмау', { onClick: () => handle.close() }),
        button('Сақтау', {
          variant: 'primary',
          onClick: async () => {
            const payload = {
              keyword: keywordInput.value.trim(),
              match_mode: modeSelect.value,
              is_active: activeBox.input.checked,
            };
            if (!payload.keyword) {
              notify.error('Триггер мәтінін енгізіңіз');
              return;
            }
            try {
              if (trigger) await api.updateTrigger(trigger.id, payload);
              else await api.createTrigger(campaignId, payload);
              notify.success('Сақталды');
              handle.close();
              await reload();
            } catch (err) {
              notify.error(err.message);
            }
          },
        }),
      ],
    });
  }

  async function deleteTrigger(trigger) {
    const confirmed = await confirmDialog({
      title: 'Триггерді жою',
      message: `«${trigger.keyword}» жойылады. Жалғастыру керек пе?`,
      confirmLabel: 'Жою',
      danger: true,
    });
    if (!confirmed) return;

    try {
      await api.deleteTrigger(trigger.id);
      notify.success('Жойылды');
      await reload();
    } catch (err) {
      notify.error(err.message);
    }
  }

  // ------------------------------------------------------------ automation --

  function automationCard() {
    const steps = campaign.steps || [];

    const body = steps.length === 0
      ? emptyState('Қадам жоқ',
          'Хабарламаларды іс-шара уақытына қатысты жоспарлаңыз: 5 сағат бұрын, 1 сағат бұрын, басталғанда.',
          button('Қадам қосу', { variant: 'primary', iconName: 'plus', onClick: () => openStepForm(null) }))
      : buildTimeline(steps);

    return card('Автоматтандыру', {
      subtitle: campaign.event_start_at
        ? `Уақыттар ${campaign.timezone} белдеуінде көрсетілген`
        : 'Іс-шара уақытын белгілегеннен кейін нақты уақыттар есептеледі',
      actions: el('div', { class: 'row' },
        button('Алдын ала қарау', { size: 'sm', iconName: 'eye', onClick: openPreview }),
        steps.length > 0 ? button('Қадам қосу', { size: 'sm', variant: 'primary', iconName: 'plus', onClick: () => openStepForm(null) }) : null),
      body,
    });
  }

  function buildTimeline(steps) {
    const timeline = el('div', { class: 'timeline' });

    // The visual order follows the actual send order, which is what the
    // operator is reasoning about, not the stored order_index.
    const ordered = [...steps].sort((a, b) => {
      if (a.schedule_kind === 'ON_TRIGGER' && b.schedule_kind !== 'ON_TRIGGER') return -1;
      if (b.schedule_kind === 'ON_TRIGGER' && a.schedule_kind !== 'ON_TRIGGER') return 1;
      return a.offset_seconds - b.offset_seconds;
    });

    for (const step of ordered) {
      const runAt = computeRunAt(step);

      timeline.append(el('div', { class: 'timeline__row' },
        el('div', { class: 'timeline__time' },
          runAt ? runAt.time : '—',
          el('small', {}, step.schedule_kind === 'ON_TRIGGER' ? 'триггер' : (runAt?.date || ''))),
        el('div', { class: 'timeline__rail' },
          el('div', { class: `timeline__dot${step.enabled ? '' : ' is-off'}` })),
        el('div', { class: `timeline__card${step.enabled ? '' : ' is-off'}` },
          el('div', { class: 'timeline__card-head' },
            el('span', { class: 'timeline__card-title' },
              step.name || offsetLabel(step.offset_seconds)),
            badge(TEMPLATE_TYPE[step.template_type] || step.template_type, 'neutral'),
            step.enabled ? null : badge('Өшірулі', 'warn'),
            el('div', { class: 'timeline__card-actions' },
              button('', {
                iconName: step.enabled ? 'pause' : 'play', size: 'sm',
                title: step.enabled ? 'Өшіру' : 'Қосу',
                onClick: () => toggleStep(step),
              }),
              button('', { iconName: 'edit', size: 'sm', title: 'Өңдеу', onClick: () => openStepForm(step) }),
              button('', { iconName: 'copy', size: 'sm', title: 'Көшіру', onClick: () => duplicateStep(step) }),
              button('', { iconName: 'trash', size: 'sm', title: 'Жою', onClick: () => deleteStep(step) }))),
          el('div', { class: 'row small subtle mb-2' },
            el('span', {}, step.schedule_kind === 'ON_TRIGGER' ? 'Триггер кезінде бірден' : offsetLabel(step.offset_seconds)),
            el('span', {}, '·'),
            el('span', { class: 'truncate' }, step.template_name)),
          step.template_preview
            ? el('div', { class: 'timeline__preview' }, step.template_preview)
            : null)));
    }

    return timeline;
  }

  function computeRunAt(step) {
    if (step.schedule_kind === 'ON_TRIGGER') return { time: 'Бірден', date: '' };
    if (!campaign.event_start_at) return null;

    const instant = new Date(new Date(campaign.event_start_at).getTime() + step.offset_seconds * 1000);
    const formatter = new Intl.DateTimeFormat('kk-KZ', {
      timeZone: campaign.timezone,
      hour: '2-digit', minute: '2-digit',
      ...(step.offset_seconds % 60 !== 0 ? { second: '2-digit' } : {}),
      hour12: false,
    });
    const dateFormatter = new Intl.DateTimeFormat('kk-KZ', {
      timeZone: campaign.timezone, day: '2-digit', month: '2-digit',
    });

    return { time: formatter.format(instant), date: dateFormatter.format(instant) };
  }

  function openStepForm(step) {
    const isEdit = Boolean(step);
    const offset = splitOffset(step?.offset_seconds ?? -3600);

    const nameInput = input({ value: step?.name || '', placeholder: 'Мысалы: 1 сағат бұрын' });

    const kindSelect = select([
      { value: 'RELATIVE_TO_EVENT', label: 'Іс-шара уақытына қатысты' },
      { value: 'ON_TRIGGER', label: 'Триггер кезінде бірден' },
    ], { value: step?.schedule_kind || 'RELATIVE_TO_EVENT' });

    const directionSelect = select([
      { value: 'before', label: 'бұрын' },
      { value: 'after', label: 'кейін' },
    ], { value: offset.direction });

    const hoursInput = input({ type: 'number', min: '0', max: '8760', value: String(offset.hours) });
    const minutesInput = input({ type: 'number', min: '0', max: '59', value: String(offset.minutes) });
    const secondsInput = input({ type: 'number', min: '0', max: '59', value: String(offset.seconds) });

    const templateSelect = select(
      templates.map((t) => ({ value: t.id, label: `${t.name} · ${TEMPLATE_TYPE[t.type] || t.type}` })),
      { value: step?.message_template_id || templates[0]?.id || '' },
    );

    const enabledBox = checkbox('Қадам қосулы', { checked: step?.enabled ?? true });

    const timingBlock = el('div', {},
      el('div', { class: 'label' }, 'Іс-шараға дейінгі/кейінгі уақыт'),
      el('div', { class: 'form-grid form-grid--3' },
        field('Сағат', hoursInput),
        field('Минут', minutesInput),
        field('Секунд', secondsInput)),
      field('Бағыты', directionSelect, {
        hint: '7.5 минут = 7 минут 30 секунд',
      }));

    const syncTiming = () => {
      timingBlock.classList.toggle('hidden', kindSelect.value === 'ON_TRIGGER');
    };
    kindSelect.addEventListener('change', syncTiming);
    syncTiming();

    if (templates.length === 0) {
      openModal({
        title: 'Шаблон қажет',
        body: el('p', { class: 'muted' },
          'Қадам қосу үшін алдымен «Шаблондар» бөлімінде кемінде бір шаблон құрыңыз.'),
        footer: [button('Түсінікті', { variant: 'primary', onClick: () => navigate('/templates') })],
      });
      return;
    }

    const handle = openModal({
      title: isEdit ? 'Қадамды өңдеу' : 'Жаңа қадам',
      wide: true,
      body: el('div', {},
        field('Қадам атауы', nameInput),
        field('Жоспарлау түрі', kindSelect),
        timingBlock,
        field('Шаблон', templateSelect, {
          hint: 'Шаблон мазмұны жіберер алдында оқылады, сондықтан кейінгі өзгертулер де қолданылады',
        }),
        el('div', { class: 'field' }, enabledBox)),
      footer: [
        button('Болдырмау', { onClick: () => handle.close() }),
        button('Сақтау', {
          variant: 'primary',
          onClick: async () => {
            const payload = {
              name: nameInput.value.trim(),
              schedule_kind: kindSelect.value,
              offset_seconds: kindSelect.value === 'ON_TRIGGER'
                ? 0
                : buildOffsetSeconds(directionSelect.value, hoursInput.value, minutesInput.value, secondsInput.value),
              message_template_id: templateSelect.value,
              enabled: enabledBox.input.checked,
            };

            try {
              if (isEdit) await api.updateStep(campaignId, step.id, payload);
              else await api.createStep(campaignId, payload);
              notify.success('Сақталды');
              handle.close();
              await reload();
            } catch (err) {
              notify.error(err.message);
            }
          },
        }),
      ],
    });
  }

  async function toggleStep(step) {
    try {
      await api.updateStep(campaignId, step.id, {
        name: step.name,
        schedule_kind: step.schedule_kind,
        offset_seconds: step.offset_seconds,
        message_template_id: step.message_template_id,
        enabled: !step.enabled,
      });
      notify.success(step.enabled ? 'Қадам өшірілді' : 'Қадам қосылды');
      await reload();
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function duplicateStep(step) {
    try {
      await api.createStep(campaignId, {
        name: `${step.name || offsetLabel(step.offset_seconds)} (көшірме)`,
        schedule_kind: step.schedule_kind,
        offset_seconds: step.offset_seconds,
        message_template_id: step.message_template_id,
        enabled: false,
      });
      notify.success('Қадам көшірілді (өшірулі күйде)');
      await reload();
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function deleteStep(step) {
    const confirmed = await confirmDialog({
      title: 'Қадамды жою',
      message: 'Осы қадам бойынша жоспарланған, әлі жіберілмеген хабарламалар тоқтатылады.',
      confirmLabel: 'Жою',
      danger: true,
    });
    if (!confirmed) return;

    try {
      await api.deleteStep(campaignId, step.id);
      notify.success('Жойылды');
      await reload();
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function openPreview() {
    let entries = [];
    try {
      entries = await api.campaignPreview(campaignId);
    } catch (err) {
      notify.error(err.message);
      return;
    }

    const body = entries.length === 0
      ? el('p', { class: 'muted' }, 'Қадамдар жоқ')
      : el('div', { class: 'table-wrap' },
          el('table', { class: 'table' },
            el('thead', {}, el('tr', {},
              el('th', {}, 'Уақыты'),
              el('th', {}, 'Қадам'),
              el('th', {}, 'Шаблон'),
              el('th', {}, 'Түрі'),
              el('th', {}, ''))),
            el('tbody', {}, ...entries.map((entry) => el('tr', {},
              el('td', { class: 'nowrap tnum' }, entry.local_time || '—'),
              el('td', {}, entry.name || entry.offset_label),
              el('td', {}, entry.template_name),
              el('td', {}, TEMPLATE_TYPE[entry.template_type] || entry.template_type),
              el('td', {},
                entry.enabled ? null : badge('Өшірулі', 'warn'),
                entry.warning ? badge(entry.warning, 'danger') : null))))));

    const handle = openModal({
      title: 'Автоматтандыру уақыты',
      wide: true,
      body: el('div', {},
        el('p', { class: 'muted small mb-3' },
          campaign.event_start_at
            ? `Іс-шара: ${formatDateTime(campaign.event_start_at)} (${campaign.timezone})`
            : 'Іс-шара уақыты белгіленбеген'),
        body),
      footer: [button('Жабу', { variant: 'primary', onClick: () => handle.close() })],
    });
  }

  async function setStatus(status) {
    try {
      await api.setCampaignStatus(campaignId, status);
      notify.success(status === 'ACTIVE' ? 'Кампания іске қосылды' : 'Кампания кідіртілді');
      await reload();
    } catch (err) {
      notify.error(err.message);
    }
  }
}
