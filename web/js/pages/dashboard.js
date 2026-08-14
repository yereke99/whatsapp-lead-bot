import { api } from '../api.js';
import { el, mount, card, icon, badge, button, emptyState } from '../ui.js';
import { formatNumber, CAMPAIGN_STATUS } from '../format.js';
import { lineChart, donut, chartColors } from '../charts.js';

export async function renderDashboard(root, { navigate } = {}) {
  mount(root, el('div', { class: 'grid grid--stats' },
    ...Array.from({ length: 8 }, () => el('div', { class: 'stat' },
      el('div', { class: 'skeleton', style: { width: '55%' } }),
      el('div', { class: 'skeleton mt-2', style: { width: '35%', height: '24px' } })))));

  const [dashboard, contactSeries, messageSeries, delivery] = await Promise.all([
    api.dashboard(),
    api.contactSeries(30),
    api.messageSeries(30),
    api.deliveryBreakdown(30),
  ]);

  const s = dashboard.summary;

  const stats = [
    { label: 'Барлық клиент', value: s.total_contacts, meta: `Бүгін +${formatNumber(s.new_contacts_today)}`, dot: '' },
    { label: 'Белсенді', value: s.active_contacts, meta: 'Автоматтандыруда', dot: '' },
    { label: 'Оқылмаған чаттар', value: s.unread_chats, meta: 'Жауап күтуде', dot: 'info' },
    { label: 'Бүгін жіберілді', value: s.messages_sent_today, meta: `Жеткізілді: ${formatNumber(s.delivered_today)}`, dot: '' },
    { label: 'Бүгін қабылданды', value: s.messages_received_today, meta: 'Кіріс хабарламалар', dot: 'info' },
    { label: 'Белсенді кампания', value: s.active_campaigns, meta: 'Триггер қабылдауда', dot: '' },
    { label: 'Кезекте', value: s.pending_scheduled_messages, meta: `Дайын: ${formatNumber(dashboard.queue.due_now)}`, dot: 'warn' },
    { label: 'Қателер', value: s.failed_messages, meta: 'Жіберілмеген', dot: s.failed_messages > 0 ? 'danger' : '' },
  ];

  const statGrid = el('div', { class: 'grid grid--stats' },
    ...stats.map((stat) => el('div', { class: 'stat' },
      el('div', { class: 'stat__label' },
        el('span', { class: `stat__dot${stat.dot ? ` stat__dot--${stat.dot}` : ''}` }),
        stat.label),
      el('div', { class: 'stat__value tnum' }, formatNumber(stat.value)),
      el('div', { class: 'stat__meta' }, stat.meta))),
  );

  const contactsCard = card('Клиенттер динамикасы', {
    subtitle: 'Соңғы 30 күн',
    body: el('div', {},
      lineChart([{ points: contactSeries, color: 'accent' }]),
      el('div', { class: 'legend' },
        legendItem(chartColors.accent, 'Жаңа клиенттер'))),
  });

  const messagesCard = card('Хабарламалар', {
    subtitle: 'Соңғы 30 күн',
    body: el('div', {},
      lineChart([
        { points: messageSeries.outgoing || [], color: 'accent' },
        { points: messageSeries.incoming || [], color: 'info' },
      ], { showArea: false }),
      el('div', { class: 'legend' },
        legendItem(chartColors.accent, 'Шығыс'),
        legendItem(chartColors.info, 'Кіріс'))),
  });

  const deliverySegments = [
    { label: 'Оқылды', value: delivery.read, color: 'accent' },
    { label: 'Жеткізілді', value: delivery.delivered, color: 'info' },
    { label: 'Жіберілді', value: delivery.sent, color: 'muted' },
    { label: 'Қате', value: delivery.failed, color: 'danger' },
  ];

  const deliveryCard = card('Жеткізу мәртебесі', {
    subtitle: 'Соңғы 30 күн',
    body: el('div', { class: 'row', style: { gap: '22px', alignItems: 'center' } },
      donut(deliverySegments),
      el('div', { class: 'stack stack--sm', style: { flex: '1', minWidth: '160px' } },
        ...deliverySegments.map((segment) => el('div', { class: 'row row--between' },
          legendItem(chartColors[segment.color], segment.label),
          el('strong', { class: 'tnum' }, formatNumber(segment.value)))))),
  });

  const campaignRows = (dashboard.campaigns || []).slice(0, 8);
  const campaignsCard = card('Кампаниялар', {
    subtitle: 'Белсенділік бойынша',
    flush: true,
    actions: navigate ? button('Барлығы', { size: 'sm', onClick: () => navigate('/campaigns') }) : null,
    body: campaignRows.length === 0
      ? emptyState('Кампания жоқ', 'Алғашқы кампанияны құрып, триггер қосыңыз.')
      : el('div', { class: 'table-wrap' },
          el('table', { class: 'table table--cards' },
            el('thead', {}, el('tr', {},
              el('th', {}, 'Кампания'),
              el('th', {}, 'Мәртебе'),
              el('th', { class: 'is-numeric' }, 'Клиент'),
              el('th', { class: 'is-numeric' }, 'Жіберілді'),
              el('th', { class: 'is-numeric' }, 'Кезекте'),
              el('th', { class: 'is-numeric' }, 'Аяқтау'))),
            el('tbody', {}, ...campaignRows.map((row) => {
              const status = CAMPAIGN_STATUS[row.status] || { label: row.status, tone: 'neutral' };
              return el('tr', {},
                el('td', { 'data-label': 'Кампания' }, el('span', { class: 'cell-primary' }, row.name)),
                el('td', { 'data-label': 'Мәртебе' }, badge(status.label, status.tone)),
                el('td', { 'data-label': 'Клиент', class: 'is-numeric tnum' }, formatNumber(row.contacts)),
                el('td', { 'data-label': 'Жіберілді', class: 'is-numeric tnum' }, formatNumber(row.messages_sent)),
                el('td', { 'data-label': 'Кезекте', class: 'is-numeric tnum' }, formatNumber(row.pending)),
                el('td', { 'data-label': 'Аяқтау', class: 'is-numeric tnum' }, `${row.completion_rate.toFixed(0)}%`),
              );
            })))),
  });

  const providerWarning = dashboard.provider?.configured
    ? null
    : el('div', { class: 'alert alert--warn' },
        icon('alert', 17),
        el('div', {},
          el('div', { class: 'alert__title' }, 'Green API қосылмаған'),
          el('div', {}, 'GREEN_API_INSTANCE_ID және GREEN_API_TOKEN айнымалыларын енгізіңіз. ' +
            'Оларсыз хабарламалар қабылданбайды және жіберілмейді.')));

  mount(root,
    providerWarning,
    statGrid,
    el('div', { class: 'grid grid--2 mt-3' }, contactsCard, messagesCard),
    el('div', { class: 'grid grid--2 mt-3' }, deliveryCard, campaignsCard),
  );
}

function legendItem(color, label) {
  return el('span', { class: 'legend__item' },
    el('span', { class: 'legend__swatch', style: { background: color } }),
    label);
}
