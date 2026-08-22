import { api } from '../api.js';
import {
  el, mount, clear, icon, button, badge, notify, emptyState,
  skeletonRows, pagination, debounce, confirmDialog, openModal, field, input, select,
} from '../ui.js';
import {
  formatPhone, formatDateTime, relativeTime, formatNumber, CONTACT_STATUS,
} from '../format.js';

export async function renderContacts(root, { navigate }) {
  const filters = {
    search: '',
    status: '',
    campaign_id: '',
    opted_out: '',
    created_from: '',
    created_to: '',
    sort: 'created_desc',
    limit: 25,
    offset: 0,
  };

  const selection = new Set();
  let campaigns = [];

  const tableBody = el('tbody', {});
  const footer = el('div', {});
  const bulkBar = el('div', { class: 'row hidden', style: { padding: '10px 18px', borderBottom: '1px solid var(--border)', background: 'var(--accent-soft)' } });

  const searchInput = input({ type: 'search', class: 'input toolbar__search', placeholder: 'Аты немесе телефон' });
  searchInput.addEventListener('input', debounce(() => {
    filters.search = searchInput.value.trim();
    filters.offset = 0;
    load();
  }, 280));

  const statusSelect = select([
    { value: '', label: 'Барлық мәртебе' },
    ...Object.entries(CONTACT_STATUS).map(([value, meta]) => ({ value, label: meta.label })),
  ], {
    onChange: (event) => { filters.status = event.target.value; filters.offset = 0; load(); },
  });

  const campaignSelect = select([{ value: '', label: 'Барлық кампания' }], {
    onChange: (event) => { filters.campaign_id = event.target.value; filters.offset = 0; load(); },
  });

  const sortSelect = select([
    { value: 'created_desc', label: 'Жаңадан ескіге' },
    { value: 'created_asc', label: 'Ескіден жаңаға' },
    { value: 'activity_desc', label: 'Соңғы белсенділік' },
    { value: 'name_asc', label: 'Аты бойынша' },
  ], {
    value: filters.sort,
    onChange: (event) => { filters.sort = event.target.value; filters.offset = 0; load(); },
  });

  const selectAll = el('input', { type: 'checkbox', 'aria-label': 'Барлығын таңдау' });
  selectAll.addEventListener('change', () => {
    for (const row of tableBody.querySelectorAll('input[data-contact-id]')) {
      row.checked = selectAll.checked;
      const id = row.dataset.contactId;
      if (selectAll.checked) selection.add(id);
      else selection.delete(id);
    }
    renderBulkBar();
  });

  const table = el('table', { class: 'table table--cards' },
    el('thead', {}, el('tr', {},
      el('th', { style: { width: '36px' } }, selectAll),
      el('th', {}, 'Клиент'),
      el('th', {}, 'Кампания'),
      el('th', {}, 'Триггер'),
      el('th', {}, 'Мәртебе'),
      el('th', { class: 'is-numeric' }, 'Хабарлама'),
      el('th', {}, 'Соңғы белсенділік'),
      el('th', {}, 'Тіркелген'),
      el('th', {}))),
    tableBody,
  );

  mount(root,
    el('div', { class: 'page-head' },
      el('div', { class: 'page-head__text' },
        el('h1', {}, 'Клиенттер'),
        el('p', { class: 'page-head__desc' },
          'Триггер жіберіп, воронкаға кірген барлық байланыс')),
      el('div', { class: 'page-head__actions' },
        button('Экспорт (CSV)', { iconName: 'download', onClick: exportCsv }))),
    el('div', { class: 'card' },
      el('div', { class: 'toolbar' },
        searchInput, statusSelect, campaignSelect, sortSelect),
      bulkBar,
      el('div', { class: 'table-wrap' }, table),
      footer),
  );

  loadCampaigns();
  await load();

  async function loadCampaigns() {
    try {
      campaigns = await api.campaigns({ include_archived: 'true' });
      clear(campaignSelect);
      campaignSelect.append(el('option', { value: '' }, 'Барлық кампания'));
      for (const campaign of campaigns) {
        campaignSelect.append(el('option', { value: campaign.id }, campaign.name));
      }
    } catch {
      // The filter is optional; a failure here should not block the list.
    }
  }

  async function load() {
    mount(tableBody, ...skeletonRows(6, 9));

    try {
      const contacts = await api.contacts(filters);
      const meta = contacts?._meta || { total: 0, limit: filters.limit, offset: filters.offset };

      clear(tableBody);
      selectAll.checked = false;

      if (!contacts || contacts.length === 0) {
        mount(tableBody, el('tr', {}, el('td', { colspan: '9' },
          emptyState('Клиент табылмады',
            'Клиент триггер жібергенде осында автоматты түрде пайда болады.'))));
        mount(footer);
        return;
      }

      for (const contact of contacts) {
        tableBody.append(contactRow(contact));
      }

      mount(footer, pagination({
        total: meta.total,
        limit: meta.limit,
        offset: meta.offset,
        onChange: (offset) => { filters.offset = offset; load(); },
      }));
    } catch (err) {
      mount(tableBody, el('tr', {}, el('td', { colspan: '9' },
        el('div', { class: 'empty' }, err.message))));
    }
  }

  function contactRow(contact) {
    const status = CONTACT_STATUS[contact.status] || { label: contact.status, tone: 'neutral' };

    const checkbox = el('input', {
      type: 'checkbox',
      dataset: { contactId: contact.id },
      checked: selection.has(contact.id) || null,
      'aria-label': 'Таңдау',
    });

    checkbox.addEventListener('change', (event) => {
      event.stopPropagation();
      if (checkbox.checked) selection.add(contact.id);
      else selection.delete(contact.id);
      renderBulkBar();
    });

    const open = () => navigate(`/contacts/${contact.id}`);

    return el('tr', { class: 'is-clickable', onClick: open },
      el('td', {
        onClick: (event) => event.stopPropagation(),
      }, checkbox),
      el('td', { 'data-label': 'Клиент' },
        el('div', { class: 'cell-primary' }, contact.name || contact.push_name || '—'),
        el('div', { class: 'cell-secondary' }, formatPhone(contact.phone))),
      el('td', { 'data-label': 'Кампания' }, contact.campaign_name || '—'),
      el('td', { 'data-label': 'Триггер' },
        el('span', { class: 'small truncate', style: { maxWidth: '220px', display: 'inline-block' } },
          contact.first_trigger_keyword || '—')),
      el('td', { 'data-label': 'Мәртебе' },
        badge(status.label, status.tone),
        contact.opted_out ? badge('STOP', 'warn') : null),
      el('td', { 'data-label': 'Хабарлама', class: 'is-numeric tnum' },
        `${formatNumber(contact.incoming_count)} / ${formatNumber(contact.outgoing_count)}`),
      el('td', { 'data-label': 'Белсенділік' }, relativeTime(contact.last_activity_at)),
      el('td', { 'data-label': 'Тіркелген' }, formatDateTime(contact.created_at)),
      el('td', { class: 'table__actions' },
        button('', { iconName: 'eye', size: 'sm', title: 'Ашу', onClick: open })),
    );
  }

  function renderBulkBar() {
    clear(bulkBar);

    if (selection.size === 0) {
      bulkBar.classList.add('hidden');
      return;
    }
    bulkBar.classList.remove('hidden');

    bulkBar.append(
      el('strong', {}, `${selection.size} таңдалды`),
      el('span', { class: 'toolbar__spacer' }),
      button('Тег қосу', { size: 'sm', onClick: () => promptTag() }),
      button('Автоматтандырудан шығару', { size: 'sm', onClick: () => runBulk('remove_from_automation', 'Таңдалған клиенттерді автоматтандырудан шығару керек пе?') }),
      button('Жазылымнан шығару', { size: 'sm', onClick: () => runBulk('unsubscribe', 'Таңдалған клиенттерді жазылымнан шығару керек пе? Барлық жоспарланған хабарламалар тоқтатылады.') }),
      button('Бұғаттау', { size: 'sm', variant: 'danger', onClick: () => runBulk('block', 'Таңдалған клиенттерді бұғаттау керек пе?') }),
      button('', { iconName: 'x', size: 'sm', variant: 'ghost', title: 'Таңдауды алып тастау', onClick: () => { selection.clear(); load(); renderBulkBar(); } }),
    );
  }

  async function runBulk(action, question) {
    const confirmed = await confirmDialog({
      title: 'Топтық әрекет',
      message: question,
      confirmLabel: 'Орындау',
      danger: action === 'block',
    });
    if (!confirmed) return;

    try {
      const result = await api.bulkAction({ contact_ids: [...selection], action });
      notify.success(`${result.affected} жазба өңделді`);
      selection.clear();
      renderBulkBar();
      await load();
    } catch (err) {
      notify.error(err.message);
    }
  }

  function promptTag() {
    const nameInput = input({ placeholder: 'Мысалы: Ыстық лид' });

    const handle = openModal({
      title: 'Тег қосу',
      body: field('Тег атауы', nameInput),
      footer: [
        button('Болдырмау', { onClick: () => handle.close() }),
        button('Қосу', {
          variant: 'primary',
          onClick: async () => {
            const tagName = nameInput.value.trim();
            if (!tagName) return;
            try {
              const result = await api.bulkAction({
                contact_ids: [...selection], action: 'tag', tag_name: tagName,
              });
              notify.success(`${result.affected} клиентке тег қойылды`);
              handle.close();
              selection.clear();
              renderBulkBar();
              await load();
            } catch (err) {
              notify.error(err.message);
            }
          },
        }),
      ],
    });
  }

  async function exportCsv() {
    const query = { ...filters };
    delete query.limit;
    delete query.offset;
    // Fetch (instead of a plain navigation) so a refusal is shown as a toast
    // with the server's message rather than dumping a raw JSON error page into
    // the browser window. The session cookie rides along automatically.
    try {
      const res = await fetch(api.exportContactsUrl(query), { credentials: 'same-origin' });
      if (!res.ok) {
        let message = 'Экспорт қол жетімді емес';
        try {
          const body = await res.json();
          if (body?.error?.message) message = body.error.message;
        } catch {
          // Error body was not JSON; keep the generic message.
        }
        notify.error(message);
        return;
      }
      const blob = await res.blob();
      const disposition = res.headers.get('Content-Disposition') || '';
      const match = disposition.match(/filename="?([^"]+)"?/i);
      const filename = match ? match[1] : 'contacts.csv';
      const url = URL.createObjectURL(blob);
      const link = el('a', { href: url, download: filename });
      document.body.append(link);
      link.click();
      link.remove();
      URL.revokeObjectURL(url);
    } catch {
      notify.error('Экспорт орындалмады');
    }
  }
}
