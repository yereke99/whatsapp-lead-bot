import { api } from '../api.js';
import {
  el, mount, clear, button, badge, notify, emptyState,
  skeletonRows, pagination, select, input, confirmDialog,
} from '../ui.js';
import { formatDateTime, formatPhone, formatNumber, JOB_STATUS } from '../format.js';
import { on } from '../store.js';

export async function renderScheduled(root, { navigate }) {
  const filters = { status: '', campaign_id: '', limit: 50, offset: 0 };

  const tableBody = el('tbody', {});
  const footer = el('div', {});
  const statsRow = el('div', { class: 'grid grid--stats mb-3' });

  const statusSelect = select([
    { value: '', label: 'Барлық мәртебе' },
    ...Object.entries(JOB_STATUS).map(([value, meta]) => ({ value, label: meta.label })),
  ], {
    onChange: (event) => { filters.status = event.target.value; filters.offset = 0; load(); },
  });

  const campaignSelect = select([{ value: '', label: 'Барлық кампания' }], {
    onChange: (event) => { filters.campaign_id = event.target.value; filters.offset = 0; load(); },
  });

  mount(root,
    el('div', { class: 'page-head' },
      el('div', { class: 'page-head__text' },
        el('h1', {}, 'Жоспарланған хабарламалар'),
        el('p', { class: 'page-head__desc' },
          'Кезек дерекқорда сақталады: сервер қайта қосылса да, тапсырмалар жоғалмайды')),
      el('div', { class: 'page-head__actions' },
        button('Жаңарту', { iconName: 'refresh', onClick: () => load() }))),
    statsRow,
    el('div', { class: 'card' },
      el('div', { class: 'toolbar' }, statusSelect, campaignSelect),
      el('div', { class: 'table-wrap' },
        el('table', { class: 'table table--cards' },
          el('thead', {}, el('tr', {},
            el('th', {}, 'Жіберу уақыты'),
            el('th', {}, 'Клиент'),
            el('th', {}, 'Кампания / қадам'),
            el('th', {}, 'Мәртебе'),
            el('th', { class: 'is-numeric' }, 'Әрекет'),
            el('th', {}, 'Мәлімет'),
            el('th', {}))),
          tableBody)),
      footer),
  );

  loadCampaigns();
  await load();

  // Job state changes arrive over the stream; a light refresh keeps the queue
  // view honest without the operator pressing anything.
  const off = on('job.updated', () => {
    clearTimeout(off.timer);
    off.timer = setTimeout(() => load(), 1500);
  });

  async function loadCampaigns() {
    try {
      const campaigns = await api.campaigns({ include_archived: 'true' });
      clear(campaignSelect);
      campaignSelect.append(el('option', { value: '' }, 'Барлық кампания'));
      for (const campaign of campaigns) {
        campaignSelect.append(el('option', { value: campaign.id }, campaign.name));
      }
    } catch {
      // Filtering by campaign is optional.
    }
  }

  async function load() {
    mount(tableBody, ...skeletonRows(6, 7));

    try {
      const [jobs, dashboard] = await Promise.all([
        api.scheduled(filters),
        api.dashboard().catch(() => null),
      ]);

      if (dashboard?.queue) renderStats(dashboard.queue);

      const meta = jobs?._meta || { total: 0, limit: filters.limit, offset: filters.offset };
      clear(tableBody);

      if (!jobs || jobs.length === 0) {
        mount(tableBody, el('tr', {}, el('td', { colspan: '7' },
          emptyState('Жазба жоқ', 'Клиент кампанияға қосылғанда тапсырмалар осында пайда болады.'))));
        mount(footer);
        return;
      }

      for (const job of jobs) tableBody.append(jobRow(job));

      mount(footer, pagination({
        total: meta.total, limit: meta.limit, offset: meta.offset,
        onChange: (offset) => { filters.offset = offset; load(); },
      }));
    } catch (err) {
      mount(tableBody, el('tr', {}, el('td', { colspan: '7' },
        el('div', { class: 'empty' }, err.message))));
    }
  }

  function renderStats(queue) {
    mount(statsRow,
      tile('Кезекте', queue.pending, ''),
      tile('Дайын', queue.due_now, queue.due_now > 0 ? 'warn' : ''),
      tile('Жіберілуде', queue.processing, ''),
      tile('Жіберілді', queue.sent, ''),
      tile('Қате', queue.failed, queue.failed > 0 ? 'danger' : ''),
      tile('Тоқтатылған', queue.cancelled, ''),
    );
  }

  function tile(label, value, tone) {
    return el('div', { class: 'stat' },
      el('div', { class: 'stat__label' },
        el('span', { class: `stat__dot${tone ? ` stat__dot--${tone}` : ''}` }), label),
      el('div', { class: 'stat__value tnum', style: { fontSize: '20px' } }, formatNumber(value)));
  }

  function jobRow(job) {
    const status = JOB_STATUS[job.status] || { label: job.status, tone: 'neutral' };
    const detail = job.last_error || job.cancel_reason || '';

    const actions = el('div', { class: 'table__actions' });
    if (job.status === 'FAILED' || job.status === 'CANCELLED') {
      actions.append(button('', {
        iconName: 'refresh', size: 'sm', title: 'Қайта жіберу',
        onClick: () => retry(job),
      }));
    }
    if (job.status === 'PENDING') {
      actions.append(button('', {
        iconName: 'x', size: 'sm', title: 'Тоқтату',
        onClick: () => cancel(job),
      }));
    }
    actions.append(button('', {
      iconName: 'eye', size: 'sm', title: 'Клиент картасы',
      onClick: () => navigate(`/contacts/${job.contact_id}`),
    }));

    return el('tr', {},
      el('td', { 'data-label': 'Уақыты', class: 'nowrap' }, formatDateTime(job.scheduled_at)),
      el('td', { 'data-label': 'Клиент' },
        el('div', { class: 'cell-primary' }, job.contact_name || '—'),
        el('div', { class: 'cell-secondary' }, formatPhone(job.contact_phone))),
      el('td', { 'data-label': 'Кампания' },
        el('div', {}, job.campaign_name),
        el('div', { class: 'cell-secondary' }, job.step_name || '—')),
      el('td', { 'data-label': 'Мәртебе' }, badge(status.label, status.tone)),
      el('td', { 'data-label': 'Әрекет', class: 'is-numeric tnum' }, String(job.attempt_count)),
      el('td', { 'data-label': 'Мәлімет' },
        detail
          ? el('span', { class: 'small muted', style: { maxWidth: '260px', display: 'inline-block' } }, detail)
          : job.sent_at ? el('span', { class: 'small subtle' }, formatDateTime(job.sent_at)) : '—'),
      el('td', {}, actions),
    );
  }

  async function retry(job) {
    try {
      await api.retryJob(job.id);
      notify.success('Қайта кезекке қойылды');
      await load();
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function cancel(job) {
    const confirmed = await confirmDialog({
      title: 'Хабарламаны тоқтату',
      message: 'Бұл жоспарланған хабарлама жіберілмейді.',
      confirmLabel: 'Тоқтату',
      danger: true,
    });
    if (!confirmed) return;

    try {
      await api.cancelJob(job.id);
      notify.success('Тоқтатылды');
      await load();
    } catch (err) {
      notify.error(err.message);
    }
  }

  return () => {
    clearTimeout(off.timer);
    off();
  };
}
