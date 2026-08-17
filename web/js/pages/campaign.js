// Campaign detail: the message queue, trigger management, the pre-activation
// checklist and the timeline preview.
//
// The queue is the heart of this page. A campaign is an ordered list of
// messages, each one a reusable template plus the moment it should go out, and
// this is where the operator decides both.

import { api } from '../api.js';
import {
  el, mount, icon, button, badge, card, notify, emptyState,
  openModal, field, input, select, checkbox, confirmDialog, linkify,
} from '../ui.js';
import {
  formatDateTime, formatNumber,
  delayLabel, splitDelay, buildDelaySeconds, stepRunAt, formatInZone,
  dateInZone, timeInZone, dayInZone,
  CAMPAIGN_STATUS, TEMPLATE_TYPE, MATCH_MODE, SCHEDULE_KIND,
} from '../format.js';
import { openCampaignForm } from './campaigns.js';

export async function renderCampaignDetail(root, { params, navigate }) {
  const campaignId = params[0];

  let campaign = null;
  let templates = [];
  let validation = null;

  await reload();

  async function reload() {
    [campaign, templates, validation] = await Promise.all([
      api.campaign(campaignId),
      api.templates().catch(() => []),
      api.campaignValidate(campaignId).catch(() => null),
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
          ? button('Тоқтату', { iconName: 'pause', onClick: () => setStatus('PAUSED') })
          : button('Іске қосу', { iconName: 'play', variant: 'primary', onClick: () => setStatus('ACTIVE') }),
        button('Баптаулар', { iconName: 'settings', onClick: () => openCampaignForm(campaign, reload) })),
    );

    const stats = el('div', { class: 'grid grid--stats' },
      statTile('Клиенттер', formatNumber(campaign.contact_count)),
      statTile('Жіберілді', formatNumber(campaign.sent_count)),
      statTile('Кезекте', formatNumber(campaign.pending_jobs)),
      statTile('Хабарламалар', formatNumber((campaign.steps || []).filter((s) => s.enabled).length)),
    );

    mount(root,
      header,
      checklistBlock(),
      limitsBlock(),
      stats,
      el('div', { class: 'mt-3' }, helpBlock()),
      el('div', { class: 'mt-3' }, triggersCard()),
      el('div', { class: 'mt-3' }, queueCard()),
    );
  }

  // ----------------------------------------------------------------- help --

  // helpBlock explains the model the page is built on, in the operator's own
  // language. It is collapsed by default and remembers that choice, so it
  // teaches once without getting in the way afterwards.
  function helpBlock() {
    const open = localStorage.getItem('campaign-help-open') !== '0';

    const details = el('details', { class: 'help', ...(open ? { open: '' } : {}) },
      el('summary', { class: 'help__summary' },
        icon('alert', 15),
        el('span', {}, 'Кампания қалай жұмыс істейді?')),
      el('div', { class: 'help__body' },
        el('ol', { class: 'help__steps' },
          helpStep('Клиент триггер сөзін жазады',
            'Төмендегі «Триггерлер» бөлімінде көрсетілген мәтінді жазған клиент қана воронкаға кіреді. '
            + 'Жүйе ешқашан бірінші болып жазбайды.'),
          helpStep('Жүйе клиентті кампанияға қосады',
            'Клиент автоматты түрде тіркеледі, ал оның барлық хабарламалары дереу кезекке жазылады. '
            + 'Кезек дерекқорда сақталады, сондықтан сервер қайта қосылса да ештеңе жоғалмайды.'),
          helpStep('Хабарламалар белгіленген уақытта жіберіледі',
            'Барлық хабарлама бір ортақ кезек арқылы, бірінен соң бірі жіберіледі. '
            + 'Бір клиентке екі хабарлама бір мезетте кетпейді.')),

        el('div', { class: 'help__divider' }),

        el('div', { class: 'help__title' }, 'Шаблон мен кезектің айырмашылығы'),
        el('div', { class: 'help__grid' },
          helpCard('Шаблон — НЕ жіберіледі',
            'Мәтін, сурет, бейне, дауыстық хабарлама. Уақыт жоқ. '
            + 'Бір шаблонды бірнеше кампанияда қайта қолдануға болады.',
            'Шаблондар бөлімінде құрылады'),
          helpCard('Кезек — ҚАШАН жіберіледі',
            'Осы беттегі тізім. Әр жол — бір шаблон және оның нақты жіберілу уақыты.',
            'Осы бетте бапталады')),

        el('div', { class: 'help__divider' }),

        el('div', { class: 'help__title' }, 'Жоспарлаудың екі тәсілі'),
        el('div', { class: 'help__grid' },
          helpCard('Нақты күн мен уақыт',
            'Хабарлама күнтізбедегі нақты сәтте кетеді: 16.08.2026, 18:00. '
            + 'Барлық клиент оны бір уақытта алады. Вебинар, эфир, іс-шара үшін ыңғайлы.',
            'Мысалы: 18:00, 19:00, 20:30, 20:52:30, 21:00'),
          helpCard('Триггерден кейінгі кідіріс',
            'Хабарлама клиенттің өзі жазған сәттен бастап саналады. '
            + 'Әр клиенттің өз кестесі болады.',
            '17:30-да жазса → 17:30:02, 17:40, 18:00. '
            + '18:10-да жазса → 18:10:02, 18:20, 18:40')),

        el('div', { class: 'help__divider' }),

        el('div', { class: 'help__title' }, 'Пайдалы білу'),
        el('ul', { class: 'help__list' },
          el('li', {}, el('b', {}, 'Ретін өзгерту: '),
            'кезектегі жолды тінтуірмен сүйреңіз. Бірақ хабарлама әрқашан өзінің белгіленген '
            + 'уақыты бойынша жіберіледі — тізімдегі орны бойынша емес.'),
          el('li', {}, el('b', {}, 'Кеш қосылған клиент: '),
            'уақыты өтіп кеткен хабарламалар оған жіберілмейді. '
            + 'Мысалы, 20:40-та қосылған клиент 18:00 мен 19:00 хабарламаларын алмайды.'),
          el('li', {}, el('b', {}, 'Тоқтату: '),
            'кампанияны тоқтатқанда кезек жоғалмайды, тек кідіреді. '
            + 'Қайта қосқанда уақыты өтіп кеткен хабарламалар баптауға сәйкес өңделеді.'),
          el('li', {}, el('b', {}, 'Өзгерту: '),
            'уақытты өзгертсеңіз, тек әлі жіберілмеген хабарламалар жаңа уақытқа көшеді. '
            + 'Жіберілгендері ешқашан өзгермейді.'),
          el('li', {}, el('b', {}, 'Жазылымнан шығу: '),
            'клиент «STOP» немесе «ТОҚТАТУ» деп жазса, оның барлық кезектегі хабарламалары '
            + 'дереу тоқтатылады.'))),
    );

    details.addEventListener('toggle', () => {
      localStorage.setItem('campaign-help-open', details.open ? '1' : '0');
    });

    return details;
  }

  function helpStep(title, text) {
    return el('li', {},
      el('div', { class: 'help__step-title' }, title),
      el('div', { class: 'help__step-text' }, text));
  }

  function helpCard(title, text, example) {
    return el('div', { class: 'help__card' },
      el('div', { class: 'help__card-title' }, title),
      el('div', { class: 'help__card-text' }, text),
      example ? el('div', { class: 'help__card-example' }, example) : null);
  }

  // ------------------------------------------------------------- checklist --

  // checklistBlock shows what still stands between the campaign and going
  // live. Blocking problems and advisory warnings are separated, because an
  // operator needs to know which ones they can decide to ignore.
  function checklistBlock() {
    const problems = validation?.problems || [];
    if (problems.length === 0) return null;

    const blocking = problems.filter((p) => p.blocking);
    const warnings = problems.filter((p) => !p.blocking);

    const group = (items, title, tone) => (items.length === 0 ? null
      : el('div', { class: `alert alert--${tone}` },
          icon('alert', 17),
          el('div', {},
            el('div', { class: 'alert__title' }, title),
            el('ul', { style: { margin: '4px 0 0', paddingLeft: '18px' } },
              ...items.map((p) => el('li', {}, p.message))))));

    return el('div', { class: 'stack stack--sm' },
      group(blocking, 'Іске қосу үшін түзетіңіз', 'danger'),
      group(warnings, 'Назар аударыңыз', 'warn'));
  }

  // limitsBlock warns when a campaign is at its own configured sending cap, so
  // a queue that has stopped moving has a visible explanation.
  function limitsBlock() {
    const notices = [];
    if (campaign.max_messages_per_hour && campaign.sent_last_hour >= campaign.max_messages_per_hour) {
      notices.push(`Сағаттық шек толды: ${campaign.sent_last_hour}/${campaign.max_messages_per_hour}. `
        + 'Кезек уақыт терезесі жаңарғанша күтеді.');
    }
    if (campaign.max_messages_per_day && campaign.sent_last_day >= campaign.max_messages_per_day) {
      notices.push(`Тәуліктік шек толды: ${campaign.sent_last_day}/${campaign.max_messages_per_day}. `
        + 'Кезек уақыт терезесі жаңарғанша күтеді.');
    }
    if (notices.length === 0) return null;

    return el('div', { class: 'alert alert--warn' },
      icon('alert', 17),
      el('div', {},
        el('div', { class: 'alert__title' }, 'Жіберу шегі'),
        ...notices.map((text) => el('div', {}, text))));
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

  // ---------------------------------------------------------------- queue --

  // queueCard renders the campaign as an ordered list of messages: what goes
  // out, in what order, and exactly when.
  //
  // Rows are shown in the operator's own order, which is what dragging
  // changes. The time next to each row is the moment it will actually be sent,
  // so a queue whose order contradicts its clock is visible immediately rather
  // than being silently reordered underneath the operator.
  function queueCard() {
    const steps = campaign.steps || [];

    const body = steps.length === 0
      ? emptyState('Хабарлама жоқ',
          'Кезекке хабарлама қосыңыз: әрқайсысы шаблон мен жіберілу уақытынан тұрады.',
          button('Хабарлама қосу', { variant: 'primary', iconName: 'plus', onClick: () => openStepForm(null) }))
      : el('div', {}, orderWarning(steps), buildQueueTable(steps));

    return card('Хабарламалар кезегі', {
      subtitle: campaign.event_start_at
        ? `Уақыттар ${campaign.timezone} белдеуінде көрсетілген · жолды сүйреп ретін өзгертіңіз`
        : 'Іс-шара уақытын белгілегеннен кейін нақты уақыттар есептеледі',
      actions: el('div', { class: 'row' },
        button('Хронология', { size: 'sm', iconName: 'clock', onClick: openTimeline }),
        button('Кезекті тексеру', { size: 'sm', iconName: 'refresh', onClick: reconcileQueue }),
        steps.length > 0
          ? button('Хабарлама қосу', { size: 'sm', variant: 'primary', iconName: 'plus', onClick: () => openStepForm(null) })
          : null),
      body,
    });
  }

  // reconcileQueue rebuilds the scheduled messages from the campaign's steps.
  //
  // The server already does this on a timer and after every edit, so this
  // button should normally report that nothing was missing. That is the point:
  // it answers "is every contact really going to get every message?" with a
  // number rather than with a reassurance.
  async function reconcileQueue() {
    try {
      const stats = await api.reconcileCampaign(campaign.id);
      const repaired = (stats.jobs_created || 0) + (stats.jobs_moved || 0) +
        (stats.jobs_cancelled || 0) + (stats.enrollments_reopened || 0);

      if (repaired === 0) {
        notify.info(
          `${stats.enrollments_checked} жазылым тексерілді, жетіспейтін хабарлама жоқ`,
          'Кезек дұрыс');
      } else {
        notify.success(
          `${stats.jobs_created} жаңа хабарлама, ${stats.jobs_moved} жылжытылды, ` +
          `${stats.enrollments_reopened} жазылым қайта ашылды`,
          'Кезек түзетілді');
      }
      await reload();
    } catch (err) {
      notify.error(err.message || 'Кезекті тексеру сәтсіз аяқталды');
    }
  }

  // orderWarning flags a queue whose listed order does not match the order it
  // will be sent in. The schedule always wins at send time; this is the point
  // where the operator finds out, with a one-click fix.
  function orderWarning(steps) {
    const timed = steps
      .filter((s) => s.enabled && s.schedule_kind !== 'ON_TRIGGER')
      .map((s) => ({ id: s.id, order: s.order_index, at: stepRunAt(campaign, s) }))
      .filter((s) => s.at);

    const outOfOrder = timed.some((s, i) => i > 0 && s.at < timed[i - 1].at);
    if (!outOfOrder) return null;

    return el('div', { class: 'alert alert--warn mb-3' },
      icon('alert', 17),
      el('div', { style: { flex: '1' } },
        el('div', { class: 'alert__title' }, 'Тізім реті жіберілу ретіне сәйкес келмейді'),
        el('div', {}, 'Хабарламалар кестеде тұрған ретімен емес, белгіленген уақыты бойынша жіберіледі.')),
      button('Уақыт бойынша реттеу', { size: 'sm', onClick: sortByTime }));
  }

  function buildQueueTable(steps) {
    const ordered = [...steps].sort((a, b) => a.order_index - b.order_index);

    const tbody = el('tbody', {});
    ordered.forEach((step, index) => tbody.append(queueRow(step, index)));

    enableRowDragging(tbody);

    return el('div', { class: 'table-wrap' },
      el('table', { class: 'table queue' },
        el('thead', {}, el('tr', {},
          el('th', { style: { width: '44px' } }, '#'),
          el('th', {}, 'Күні'),
          el('th', {}, 'Уақыты'),
          el('th', {}, 'Шаблон'),
          el('th', {}, 'Түрі'),
          el('th', {}, 'Статус'),
          el('th', { class: 'is-numeric' }, ''))),
        tbody));
  }

  function queueRow(step, index) {
    const runAt = stepRunAt(campaign, step);
    const onTrigger = step.schedule_kind === 'ON_TRIGGER';
    const showSeconds = (step.offset_seconds || 0) % 60 !== 0;

    const timeCell = onTrigger
      ? el('span', { class: 'nowrap' }, `+${delayLabel(step.offset_seconds)}`)
      : el('span', { class: 'nowrap tnum' },
          runAt
            ? formatInZone(runAt, campaign.timezone, {
                hour: '2-digit',
                minute: '2-digit',
                ...(showSeconds ? { second: '2-digit' } : {}),
              })
            : '—');

    const dateCell = onTrigger
      ? el('span', { class: 'subtle small' }, 'триггер бойынша')
      : el('span', { class: 'nowrap tnum' }, runAt ? dayInZone(runAt, campaign.timezone) : '—');

    const row = el('tr', {
      class: step.enabled ? '' : 'is-off',
      draggable: 'true',
      'data-step-id': step.id,
    },
      el('td', { class: 'queue__handle', title: 'Ретін өзгерту үшін сүйреңіз' },
        el('span', { class: 'tnum' }, String(index + 1))),
      el('td', { 'data-label': 'Күні' }, dateCell),
      el('td', { 'data-label': 'Уақыты' }, timeCell),
      el('td', { 'data-label': 'Шаблон' },
        el('div', { class: 'truncate' }, step.name || step.template_name),
        step.name
          ? el('div', { class: 'small subtle truncate' }, step.template_name)
          : null),
      el('td', { 'data-label': 'Түрі' },
        badge(TEMPLATE_TYPE[step.template_type] || step.template_type, 'neutral')),
      el('td', { 'data-label': 'Статус' },
        step.enabled ? badge('Қосулы', 'success') : badge('Өшірулі', 'neutral'),
        // A restricted step reaches only part of the audience, which is the
        // kind of thing an operator must be able to see without opening the
        // form -- otherwise "why did only some people get this?" has no answer
        // on the screen where the question comes up.
        step.audience_filter_enabled && step.audience_min_joined_at
          ? el('div', { class: 'small subtle nowrap', title:
              'Тек ' + formatInZone(new Date(step.audience_min_joined_at), campaign.timezone, {
                day: '2-digit', month: '2-digit', year: 'numeric',
                hour: '2-digit', minute: '2-digit',
              }) + ' немесе одан кейін қосылған клиенттерге жіберіледі' },
              'тек жаңа қосылғандарға')
          : null),
      el('td', {},
        el('div', { class: 'table__actions' },
          button('', { iconName: 'eye', size: 'sm', title: 'Алдын ала қарау', onClick: () => openStepPreview(step) }),
          button('', { iconName: 'edit', size: 'sm', title: 'Өзгерту', onClick: () => openStepForm(step) }),
          button('', { iconName: 'copy', size: 'sm', title: 'Көшіру', onClick: () => duplicateStep(step) }),
          button('', {
            iconName: step.enabled ? 'pause' : 'play',
            size: 'sm',
            title: step.enabled ? 'Өшіру' : 'Қосу',
            onClick: () => toggleStep(step),
          }),
          button('', { iconName: 'trash', size: 'sm', title: 'Жою', onClick: () => deleteStep(step) }))),
    );

    return row;
  }

  // enableRowDragging wires HTML5 drag and drop over the queue body and saves
  // the new order once the row is dropped.
  function enableRowDragging(tbody) {
    let dragged = null;

    tbody.addEventListener('dragstart', (event) => {
      const row = event.target.closest('tr');
      if (!row) return;
      dragged = row;
      row.classList.add('is-dragging');
      event.dataTransfer.effectAllowed = 'move';
      // Firefox refuses to start a drag without payload.
      event.dataTransfer.setData('text/plain', row.dataset.stepId || '');
    });

    tbody.addEventListener('dragover', (event) => {
      if (!dragged) return;
      event.preventDefault();
      event.dataTransfer.dropEffect = 'move';

      const target = event.target.closest('tr');
      if (!target || target === dragged) return;

      // Drop above or below the row the pointer is over, depending on which
      // half it is in, so the insertion point follows the cursor.
      const box = target.getBoundingClientRect();
      const below = event.clientY > box.top + box.height / 2;
      target.parentNode.insertBefore(dragged, below ? target.nextSibling : target);
    });

    tbody.addEventListener('dragend', async () => {
      if (!dragged) return;
      dragged.classList.remove('is-dragging');
      dragged = null;

      const ids = [...tbody.querySelectorAll('tr[data-step-id]')].map((row) => row.dataset.stepId);
      await saveOrder(ids);
    });
  }

  async function saveOrder(ids) {
    const current = [...(campaign.steps || [])]
      .sort((a, b) => a.order_index - b.order_index)
      .map((s) => s.id);
    if (ids.length === current.length && ids.every((id, i) => id === current[i])) return;

    try {
      await api.reorderSteps(campaignId, ids);
      notify.success('Рет сақталды');
      await reload();
    } catch (err) {
      notify.error(err.message);
      await reload();
    }
  }

  // sortByTime rewrites the stored order so the list reads in send order.
  // Trigger-anchored messages come first, ordered by their delay.
  async function sortByTime() {
    const ids = [...(campaign.steps || [])]
      .sort((a, b) => {
        const aTrigger = a.schedule_kind === 'ON_TRIGGER';
        const bTrigger = b.schedule_kind === 'ON_TRIGGER';
        if (aTrigger !== bTrigger) return aTrigger ? -1 : 1;
        return a.offset_seconds - b.offset_seconds;
      })
      .map((s) => s.id);

    await saveOrder(ids);
  }

  // ------------------------------------------------------------ step form --

  // openStepForm builds one entry of the queue: the content to send, and when
  // to send it. The two scheduling modes answer different questions, so the
  // form shows only the fields belonging to the chosen one.
  function openStepForm(step) {
    if (templates.length === 0) {
      const handle = openModal({
        title: 'Шаблон қажет',
        body: el('p', { class: 'muted' },
          'Хабарлама қосу үшін алдымен «Шаблондар» бөлімінде кемінде бір шаблон құрыңыз.'),
        footer: [button('Шаблондарға өту', { variant: 'primary', onClick: () => { handle.close(); navigate('/templates'); } })],
      });
      return;
    }

    const isEdit = Boolean(step);
    const onTrigger = step?.schedule_kind === 'ON_TRIGGER';

    const nameInput = input({
      value: step?.name || '',
      placeholder: 'Мысалы: 1 сағат қалды',
    });

    const kindSelect = select(
      Object.entries(SCHEDULE_KIND).map(([value, label]) => ({ value, label })),
      { value: step?.schedule_kind || 'RELATIVE_TO_EVENT' },
    );

    // Exact mode: a date and a time in the campaign's timezone. Seconds are
    // part of the picker because a step at 20:52:30 is a legitimate schedule.
    const runAt = isEdit && !onTrigger ? stepRunAt(campaign, step) : null;
    const eventStart = campaign.event_start_at ? new Date(campaign.event_start_at) : null;

    const dateInput = input({
      type: 'date',
      value: dateInZone(runAt || eventStart, campaign.timezone),
    });
    const timeInput = input({
      type: 'time',
      step: '1',
      value: timeInZone(runAt || eventStart, campaign.timezone) || '21:00:00',
    });

    // Delay mode: how long after the customer's own message.
    const delay = splitDelay(onTrigger ? step.offset_seconds : 0);
    const delayHours = input({ type: 'number', min: '0', max: '8760', value: String(delay.hours) });
    const delayMinutes = input({ type: 'number', min: '0', max: '59', value: String(delay.minutes) });
    const delaySeconds = input({ type: 'number', min: '0', max: '59', value: String(delay.seconds) });

    const templateSelect = select(
      templates.map((t) => ({ value: t.id, label: `${t.name} · ${TEMPLATE_TYPE[t.type] || t.type}` })),
      { value: step?.message_template_id || templates[0]?.id || '' },
    );

    const enabledBox = checkbox('Хабарлама қосулы', { checked: step?.enabled ?? true });

    // Audience cutoff. The case this exists for is the last message before an
    // event: "we are gathering, come in" is a welcome to somebody who signed up
    // two minutes ago and a repetition to somebody who has been getting
    // reminders since morning. Restricting one step lets both be true.
    const audienceCutoff = step?.audience_min_joined_at
      ? new Date(step.audience_min_joined_at)
      : (runAt || eventStart);

    const audienceBox = checkbox('Тек белгілі уақыттан кейін қосылғандарға жіберу', {
      checked: step?.audience_filter_enabled ?? false,
    });
    const audienceDate = input({
      type: 'date',
      value: dateInZone(audienceCutoff, campaign.timezone),
    });
    const audienceTime = input({
      type: 'time',
      step: '1',
      value: timeInZone(audienceCutoff, campaign.timezone) || '20:55:00',
    });

    const audienceFields = el('div', {},
      el('p', { class: 'muted small' },
        'Осы хабарламаны тек көрсетілген күн мен уақытта немесе одан кейін кампанияға '
        + 'қосылған клиенттер алады. Бұрын қосылғандарға бұл хабарлама жіберілмейді.'),
      el('div', { class: 'form-grid' },
        field('Күні', audienceDate),
        field('Уақыты', audienceTime, { hint: `${campaign.timezone} белдеуі бойынша, осы сәтті қоса` })));

    const syncAudience = () => {
      audienceFields.classList.toggle('hidden', !audienceBox.input.checked);
    };
    audienceBox.input.addEventListener('change', syncAudience);
    syncAudience();

    const audienceBlock = el('div', { class: 'field' },
      audienceBox,
      audienceFields);

    const exactBlock = el('div', {},
      el('p', { class: 'muted small' },
        'Хабарлама күнтізбедегі осы нақты сәтте жіберіледі. Барлық клиент оны бір уақытта алады. '
        + 'Секундты да көрсетуге болады, мысалы 20:52:30.'),
      el('div', { class: 'form-grid' },
        field('Күні', dateInput),
        field('Уақыты', timeInput, { hint: `${campaign.timezone} белдеуі бойынша` })),
      campaign.event_start_at
        ? null
        : el('div', { class: 'alert alert--warn' },
            icon('alert', 16),
            el('div', {}, 'Алдымен кампания баптауларында іс-шара күні мен уақытын белгілеңіз.')));

    const delayBlock = el('div', {},
      el('p', { class: 'muted small' },
        'Хабарлама клиент триггер сөзін жазған сәттен бастап есептеледі. '
        + 'Әр клиенттің өз кестесі болады: 17:30-да жазған клиент 17:40-та алса, '
        + '18:10-да жазған клиент 18:20-да алады.'),
      el('div', { class: 'label' }, 'Триггерден кейін қанша уақыттан соң'),
      el('div', { class: 'form-grid form-grid--3' },
        field('Сағат', delayHours),
        field('Минут', delayMinutes),
        field('Секунд', delaySeconds)),
      el('p', { class: 'muted small' },
        'Нөл қалдырсаңыз, хабарлама триггерден кейін бірнеше секундтан соң жіберіледі — '
        + 'бірден емес, сондықтан жазысу табиғи көрінеді.'));

    const syncMode = () => {
      const isDelay = kindSelect.value === 'ON_TRIGGER';
      exactBlock.classList.toggle('hidden', isDelay);
      delayBlock.classList.toggle('hidden', !isDelay);
    };
    kindSelect.addEventListener('change', syncMode);
    syncMode();

    const errorBox = el('div', { class: 'alert alert--danger hidden' });

    const handle = openModal({
      title: isEdit ? 'Хабарламаны өзгерту' : 'Жаңа хабарлама',
      wide: true,
      body: el('div', {},
        errorBox,
        field('Атауы', nameInput, { hint: 'Кезекте көрінетін қысқа белгі, мысалы «1 сағат қалды»' }),
        field('Шаблон', templateSelect, {
          hint: 'Не жіберілетінін шаблон шешеді, қашан жіберілетінін — осы терезе. '
            + 'Бір шаблонды бірнеше кампанияда қайта қолдануға болады.',
        }),
        field('Жоспарлау түрі', kindSelect, {
          hint: 'Вебинар/эфир үшін — нақты күн мен уақыт. Клиент жазған сәттен санау үшін — кідіріс.',
        }),
        exactBlock,
        delayBlock,
        audienceBlock,
        el('div', { class: 'field' }, enabledBox)),
      footer: [
        button('Болдырмау', { onClick: () => handle.close() }),
        button('Сақтау', { variant: 'primary', onClick: save }),
      ],
    });

    async function save() {
      errorBox.classList.add('hidden');

      const payload = {
        name: nameInput.value.trim(),
        schedule_kind: kindSelect.value,
        message_template_id: templateSelect.value,
        enabled: enabledBox.input.checked,
        offset_seconds: 0,
        audience_filter_enabled: audienceBox.input.checked,
      };

      if (audienceBox.input.checked) {
        if (!audienceDate.value) {
          showError('Аудитория шектеуі үшін күнді таңдаңыз');
          return;
        }
        // Sent as a local wall-clock moment plus a zone; the server converts to
        // UTC exactly as it does for the step's own send time.
        payload.audience_joined_date = audienceDate.value;
        payload.audience_joined_time = audienceTime.value || '00:00:00';
        payload.audience_timezone = campaign.timezone;
      }

      if (kindSelect.value === 'ON_TRIGGER') {
        payload.offset_seconds = buildDelaySeconds(
          delayHours.value, delayMinutes.value, delaySeconds.value);
      } else {
        if (!dateInput.value) {
          showError('Күнін таңдаңыз');
          return;
        }
        // The server converts the wall-clock moment into an offset from the
        // event using the campaign's own timezone, so the browser's zone
        // never enters the calculation.
        payload.scheduled_date = dateInput.value;
        payload.scheduled_time = timeInput.value || '00:00:00';
      }

      try {
        if (isEdit) await api.updateStep(campaignId, step.id, payload);
        else await api.createStep(campaignId, payload);
        notify.success('Сақталды');
        handle.close();
        await reload();
      } catch (err) {
        showError(err.message);
      }
    }

    function showError(message) {
      errorBox.textContent = message;
      errorBox.classList.remove('hidden');
    }
  }

  // --------------------------------------------------------- row actions --

  async function toggleStep(step) {
    try {
      await api.updateStep(campaignId, step.id, {
        name: step.name,
        schedule_kind: step.schedule_kind,
        offset_seconds: step.offset_seconds,
        message_template_id: step.message_template_id,
        enabled: !step.enabled,
      });
      notify.success(step.enabled ? 'Хабарлама өшірілді' : 'Хабарлама қосылды');
      await reload();
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function duplicateStep(step) {
    try {
      await api.createStep(campaignId, {
        name: `${step.name || step.template_name} (көшірме)`,
        schedule_kind: step.schedule_kind,
        offset_seconds: step.offset_seconds,
        message_template_id: step.message_template_id,
        enabled: false,
      });
      notify.success('Көшірілді (өшірулі күйде)');
      await reload();
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function deleteStep(step) {
    const confirmed = await confirmDialog({
      title: 'Хабарламаны жою',
      message: 'Осы қадам бойынша жоспарланған, әлі жіберілмеген хабарламалар тоқтатылады. Жіберілгендері өзгермейді.',
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

  // ---------------------------------------------------------------- views --

  // openStepPreview renders the message as the customer will receive it, with
  // this campaign's own values substituted for the template variables.
  async function openStepPreview(step) {
    const runAt = stepRunAt(campaign, step);
    const overrides = {
      campaign_name: campaign.name,
      webinar_link: campaign.webinar_link || '',
      timezone: campaign.timezone,
    };
    if (campaign.event_start_at) {
      const start = new Date(campaign.event_start_at);
      overrides.webinar_date = dayInZone(start, campaign.timezone);
      overrides.webinar_time = formatInZone(start, campaign.timezone,
        { hour: '2-digit', minute: '2-digit' });
      overrides.webinar_datetime = `${overrides.webinar_date} ${overrides.webinar_time}`;
    }

    let preview;
    try {
      preview = await api.templatePreview(step.message_template_id, overrides);
    } catch (err) {
      notify.error(err.message);
      return;
    }

    const bubble = el('div', { class: 'bubble bubble--out' });

    if (preview.media_url) {
      if (preview.media_kind === 'IMAGE') {
        bubble.append(el('div', { class: 'bubble__media' }, el('img', { src: preview.media_url, alt: '' })));
      } else if (preview.media_kind === 'VIDEO') {
        bubble.append(el('div', { class: 'bubble__media' }, el('video', { src: preview.media_url, controls: true })));
      } else if (preview.media_kind === 'VOICE' || preview.media_kind === 'AUDIO') {
        bubble.append(el('div', { class: 'bubble__media' }, el('audio', { src: preview.media_url, controls: true })));
      } else {
        bubble.append(el('div', { class: 'bubble__file' },
          icon('file', 18), el('div', { class: 'bubble__file-name' }, preview.media_name || 'Құжат')));
      }
    }
    if (preview.rendered_text) {
      bubble.append(el('div', { class: 'bubble__text' }, linkify(preview.rendered_text)));
    }

    const when = step.schedule_kind === 'ON_TRIGGER'
      ? `Триггерден кейін +${delayLabel(step.offset_seconds)}`
      : (runAt
        ? formatInZone(runAt, campaign.timezone, {
            day: '2-digit', month: '2-digit', year: 'numeric',
            hour: '2-digit', minute: '2-digit', second: '2-digit',
          })
        : 'уақыты белгіленбеген');

    bubble.append(el('div', { class: 'bubble__meta' },
      el('span', {}, step.schedule_kind === 'ON_TRIGGER' ? '—' : (runAt
        ? formatInZone(runAt, campaign.timezone, { hour: '2-digit', minute: '2-digit' })
        : '—')),
      icon('check', 13)));

    const handle = openModal({
      title: `${step.name || step.template_name} · алдын ала қарау`,
      wide: true,
      body: el('div', {},
        el('div', { class: 'row small subtle mb-3' },
          badge(TEMPLATE_TYPE[step.template_type] || step.template_type, 'neutral'),
          el('span', {}, when),
          el('span', {}, '·'),
          el('span', {}, `v${step.template_version || 1}`)),
        el('div', { class: 'preview-phone' }, bubble),
        preview.unknown_variables?.length
          ? el('div', { class: 'alert alert--warn mt-3' },
              icon('alert', 16),
              el('div', {},
                el('div', { class: 'alert__title' }, 'Белгісіз айнымалылар'),
                el('div', {}, preview.unknown_variables.map((v) => `{{${v}}}`).join(', ')
                  + ' — бұлар жіберер алдында алынып тасталады.')))
          : null),
      footer: [button('Жабу', { variant: 'primary', onClick: () => handle.close() })],
    });
  }

  // openTimeline is the pre-activation read of the whole campaign: every
  // message in send order, on one vertical axis.
  async function openTimeline() {
    let entries = [];
    try {
      entries = await api.campaignTimeline(campaignId);
    } catch (err) {
      notify.error(err.message);
      return;
    }

    const body = entries.length === 0
      ? el('p', { class: 'muted' }, 'Хабарламалар жоқ')
      : el('div', { class: 'timeline' },
          ...entries.map((entry) => el('div', { class: 'timeline__row' },
            el('div', { class: 'timeline__time' },
              entry.schedule_kind === 'ON_TRIGGER' ? entry.offset_label : (entry.local_time || '—'),
              el('small', {},
                entry.schedule_kind === 'ON_TRIGGER' ? 'триггерден кейін' : (entry.local_date || ''))),
            el('div', { class: 'timeline__rail' },
              el('div', { class: `timeline__dot${entry.enabled ? '' : ' is-off'}` })),
            el('div', { class: `timeline__card${entry.enabled ? '' : ' is-off'}` },
              el('div', { class: 'timeline__card-head' },
                el('span', { class: 'timeline__card-title' }, entry.name || entry.template_name),
                badge(TEMPLATE_TYPE[entry.template_type] || entry.template_type, 'neutral'),
                entry.enabled ? null : badge('Өшірулі', 'warn')),
              el('div', { class: 'row small subtle' },
                el('span', { class: 'truncate' }, entry.template_name)),
              entry.warning ? el('div', { class: 'small mt-2' }, badge(entry.warning, 'danger')) : null))));

    const handle = openModal({
      title: 'Хабарламалар хронологиясы',
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
