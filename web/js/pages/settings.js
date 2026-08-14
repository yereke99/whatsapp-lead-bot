import { api } from '../api.js';
import {
  el, mount, clear, icon, button, badge, card, notify, emptyState,
  openModal, field, input, select, checkbox, confirmDialog, pagination, debounce,
} from '../ui.js';
import { formatDateTime, formatBytes, AUDIT_ACTIONS } from '../format.js';
import { state } from '../store.js';

export async function renderSettings(root) {
  const tabs = [
    { id: 'system', label: 'Жүйе' },
    { id: 'admins', label: 'Әкімшілер' },
    { id: 'audit', label: 'Аудит журналы' },
    { id: 'webhooks', label: 'Webhook оқиғалары' },
    { id: 'account', label: 'Менің тіркелгім' },
  ];

  let activeTab = 'system';
  const tabBar = el('div', { class: 'row mb-3' });
  const panel = el('div', {});

  function renderTabs() {
    clear(tabBar);
    for (const tab of tabs) {
      if (tab.id === 'admins' && state.admin?.role !== 'OWNER') continue;

      tabBar.append(button(tab.label, {
        variant: tab.id === activeTab ? 'primary' : '',
        size: 'sm',
        onClick: () => { activeTab = tab.id; renderTabs(); renderPanel(); },
      }));
    }
  }

  async function renderPanel() {
    mount(panel, el('div', { class: 'card' }, el('div', { class: 'card__body' },
      el('div', { class: 'skeleton', style: { height: '40px' } }))));

    try {
      if (activeTab === 'system') await renderSystem(panel);
      else if (activeTab === 'admins') await renderAdmins(panel);
      else if (activeTab === 'audit') await renderAudit(panel);
      else if (activeTab === 'webhooks') await renderWebhooks(panel);
      else await renderAccount(panel);
    } catch (err) {
      mount(panel, el('div', { class: 'alert alert--danger' }, err.message));
    }
  }

  mount(root,
    el('div', { class: 'page-head' },
      el('div', { class: 'page-head__text' },
        el('h1', {}, 'Баптаулар'),
        el('p', { class: 'page-head__desc' }, 'Жүйе күйі, операторлар және оқиғалар журналы'))),
    tabBar,
    panel,
  );

  renderTabs();
  await renderPanel();
}

// ------------------------------------------------------------------ system --

async function renderSystem(panel) {
  const [settings, provider] = await Promise.all([
    api.systemSettings(),
    api.providerState().catch(() => ({ configured: false, state: 'error' })),
  ]);

  const providerTone = provider.authorized ? 'success' : provider.configured ? 'warn' : 'neutral';
  const providerLabel = !provider.configured
    ? 'Қосылмаған'
    : provider.authorized ? 'Авторизацияланған' : (provider.state || 'белгісіз');

  mount(panel,
    el('div', { class: 'grid grid--2' },
      card('WhatsApp қосылымы', {
        body: el('div', {},
          el('dl', { class: 'kv' },
            el('dt', {}, 'Провайдер'), el('dd', {}, 'Green API'),
            el('dt', {}, 'Мәртебе'), el('dd', {}, badge(providerLabel, providerTone)),
            el('dt', {}, 'Инстанс күйі'), el('dd', { class: 'mono' }, provider.state || '—')),
          provider.message ? el('div', { class: 'alert alert--warn mt-3' }, provider.message) : null,
          !provider.configured
            ? el('div', { class: 'alert alert--warn mt-3' },
                icon('alert', 16),
                el('div', {},
                  el('div', { class: 'alert__title' }, 'Баптау қажет'),
                  el('div', {}, 'GREEN_API_INSTANCE_ID, GREEN_API_TOKEN және GREEN_API_WEBHOOK_TOKEN ' +
                    'айнымалыларын енгізіп, серверді қайта іске қосыңыз.')))
            : null,
          el('div', { class: 'alert alert--info mt-3' },
            icon('link', 16),
            el('div', {},
              el('div', { class: 'alert__title' }, 'Webhook мекенжайы'),
              el('div', { class: 'mono', style: { wordBreak: 'break-all' } },
                `${window.location.origin}/api/webhooks/greenapi`),
              el('div', { class: 'small mt-2' },
                'Green API кабинетінде осы мекенжайды көрсетіңіз және webhook токенін қосыңыз.')))),
      }),
      card('Жүйе параметрлері', {
        body: el('dl', { class: 'kv' },
          el('dt', {}, 'Уақыт белдеуі'), el('dd', {}, settings.timezone),
          el('dt', {}, 'Орта'), el('dd', {}, settings.environment),
          el('dt', {}, 'Дауыстық хабарлама'), el('dd', {},
            settings.voice_supported
              ? badge('ffmpeg қолжетімді', 'success')
              : badge('ffmpeg жоқ — тек OGG/Opus', 'warn')),
          el('dt', {}, 'Планировщик'), el('dd', {},
            settings.scheduler_enabled ? badge('Қосулы', 'success') : badge('Өшірулі', 'danger')),
          el('dt', {}, 'Файл шегі'), el('dd', {}, `${settings.max_upload_mb} МБ`),
          el('dt', {}, 'Нақты уақыт'), el('dd', {}, `${settings.realtime_clients} қосылым`)),
      })),
    el('div', { class: 'mt-3' },
      card('Шаблон айнымалылары', {
        subtitle: 'Хабарлама мәтінінде қолдануға болатын белгілер',
        flush: true,
        body: el('div', { class: 'table-wrap' },
          el('table', { class: 'table table--cards' },
            el('thead', {}, el('tr', {},
              el('th', {}, 'Айнымалы'), el('th', {}, 'Атауы'),
              el('th', {}, 'Сипаттама'), el('th', {}, 'Мысал'))),
            el('tbody', {}, ...(settings.template_variables || []).map((variable) => el('tr', {},
              el('td', { 'data-label': 'Айнымалы' }, el('code', { class: 'mono' }, `{{${variable.key}}}`)),
              el('td', { 'data-label': 'Атауы' }, variable.label),
              el('td', { 'data-label': 'Сипаттама', class: 'small muted' }, variable.description),
              el('td', { 'data-label': 'Мысал', class: 'small' }, variable.example)))))),
      })),
  );
}

// ------------------------------------------------------------------ admins --

async function renderAdmins(panel) {
  const admins = await api.admins();

  const rows = admins.map((admin) => el('tr', {},
    el('td', { 'data-label': 'Email' },
      el('div', { class: 'cell-primary' }, admin.email),
      admin.name ? el('div', { class: 'cell-secondary' }, admin.name) : null),
    el('td', { 'data-label': 'Рөл' }, badge(roleLabel(admin.role), admin.role === 'OWNER' ? 'success' : 'neutral')),
    el('td', { 'data-label': 'Мәртебе' },
      admin.is_active ? badge('Белсенді', 'success') : badge('Өшірулі', 'neutral')),
    el('td', { 'data-label': 'Соңғы кіру' }, formatDateTime(admin.last_login_at)),
    el('td', { class: 'table__actions' },
      button('', { iconName: 'edit', size: 'sm', title: 'Өңдеу', onClick: () => openAdminForm(admin) }),
      admin.id === state.admin?.id
        ? null
        : button('', { iconName: 'trash', size: 'sm', title: 'Жою', onClick: () => removeAdmin(admin) })),
  ));

  mount(panel, card('Әкімшілер', {
    actions: button('Қосу', { size: 'sm', iconName: 'plus', variant: 'primary', onClick: () => openAdminForm(null) }),
    flush: true,
    body: el('div', { class: 'table-wrap' },
      el('table', { class: 'table table--cards' },
        el('thead', {}, el('tr', {},
          el('th', {}, 'Email'), el('th', {}, 'Рөл'),
          el('th', {}, 'Мәртебе'), el('th', {}, 'Соңғы кіру'), el('th', {}))),
        el('tbody', {}, ...rows))),
  }));

  function openAdminForm(admin) {
    const isEdit = Boolean(admin);

    const emailInput = input({ type: 'email', value: admin?.email || '', disabled: isEdit || null });
    const nameInput = input({ value: admin?.name || '' });
    const passwordInput = input({ type: 'password', autocomplete: 'new-password' });
    const roleSelect = select([
      { value: 'ADMIN', label: 'Әкімші — толық қолжетімділік' },
      { value: 'VIEWER', label: 'Бақылаушы — тек оқу' },
      { value: 'OWNER', label: 'Иесі — әкімшілерді басқарады' },
    ], { value: admin?.role || 'ADMIN' });
    const activeBox = checkbox('Тіркелгі белсенді', { checked: admin?.is_active ?? true });

    const handle = openModal({
      title: isEdit ? 'Әкімшіні өңдеу' : 'Жаңа әкімші',
      body: el('div', {},
        field('Email', emailInput),
        field('Аты', nameInput),
        isEdit ? null : field('Құпия сөз', passwordInput, {
          hint: 'Кемінде 10 таңба, әріп пен сан немесе таңба',
        }),
        field('Рөл', roleSelect),
        el('div', { class: 'field' }, activeBox)),
      footer: [
        button('Болдырмау', { onClick: () => handle.close() }),
        button('Сақтау', {
          variant: 'primary',
          onClick: async () => {
            try {
              if (isEdit) {
                await api.updateAdmin(admin.id, {
                  name: nameInput.value.trim(),
                  role: roleSelect.value,
                  is_active: activeBox.input.checked,
                });
              } else {
                await api.createAdmin({
                  email: emailInput.value.trim(),
                  name: nameInput.value.trim(),
                  password: passwordInput.value,
                  role: roleSelect.value,
                });
              }
              notify.success('Сақталды');
              handle.close();
              await renderAdmins(panel);
            } catch (err) {
              notify.error(err.message);
            }
          },
        }),
      ],
    });
  }

  async function removeAdmin(admin) {
    const confirmed = await confirmDialog({
      title: 'Әкімшіні жою',
      message: `${admin.email} тіркелгісі жойылады және барлық сессиялары тоқтатылады.`,
      confirmLabel: 'Жою',
      danger: true,
    });
    if (!confirmed) return;

    try {
      await api.deleteAdmin(admin.id);
      notify.success('Жойылды');
      await renderAdmins(panel);
    } catch (err) {
      notify.error(err.message);
    }
  }
}

function roleLabel(role) {
  return { OWNER: 'Иесі', ADMIN: 'Әкімші', VIEWER: 'Бақылаушы' }[role] || role;
}

// ------------------------------------------------------------------- audit --

async function renderAudit(panel) {
  const filters = { search: '', limit: 50, offset: 0 };

  const tableBody = el('tbody', {});
  const footer = el('div', {});

  const searchInput = input({ type: 'search', class: 'input toolbar__search', placeholder: 'Әрекет немесе email' });
  searchInput.addEventListener('input', debounce(() => {
    filters.search = searchInput.value.trim();
    filters.offset = 0;
    load();
  }, 280));

  mount(panel, el('div', { class: 'card' },
    el('div', { class: 'toolbar' }, searchInput),
    el('div', { class: 'table-wrap' },
      el('table', { class: 'table table--cards' },
        el('thead', {}, el('tr', {},
          el('th', {}, 'Уақыты'), el('th', {}, 'Әкімші'),
          el('th', {}, 'Әрекет'), el('th', {}, 'Мәлімет'), el('th', {}, 'IP'))),
        tableBody)),
    footer));

  await load();

  async function load() {
    try {
      const logs = await api.auditLogs(filters);
      const meta = logs?._meta || { total: 0, limit: filters.limit, offset: filters.offset };

      clear(tableBody);

      if (!logs || logs.length === 0) {
        mount(tableBody, el('tr', {}, el('td', { colspan: '5' },
          emptyState('Жазба жоқ', 'Әкімшілердің әрекеттері осында тіркеледі.'))));
        return;
      }

      for (const log of logs) {
        tableBody.append(el('tr', {},
          el('td', { 'data-label': 'Уақыты', class: 'nowrap' }, formatDateTime(log.created_at)),
          el('td', { 'data-label': 'Әкімші' }, log.admin_email || '—'),
          el('td', { 'data-label': 'Әрекет' }, badge(AUDIT_ACTIONS[log.action] || log.action, toneForAction(log.action))),
          el('td', { 'data-label': 'Мәлімет', class: 'small' }, log.summary || '—'),
          el('td', { 'data-label': 'IP', class: 'mono small' }, log.ip_address || '—')));
      }

      mount(footer, pagination({
        total: meta.total, limit: meta.limit, offset: meta.offset,
        onChange: (offset) => { filters.offset = offset; load(); },
      }));
    } catch (err) {
      mount(tableBody, el('tr', {}, el('td', { colspan: '5' }, el('div', { class: 'empty' }, err.message))));
    }
  }
}

function toneForAction(action) {
  if (action.includes('deleted') || action.includes('failed') || action.includes('blocked')) return 'danger';
  if (action.includes('created') || action.includes('activated')) return 'success';
  if (action.includes('paused') || action.includes('unsubscribed')) return 'warn';
  return 'neutral';
}

// ---------------------------------------------------------------- webhooks --

async function renderWebhooks(panel) {
  const filters = { status: '', limit: 50, offset: 0 };

  const tableBody = el('tbody', {});
  const footer = el('div', {});

  const statusSelect = select([
    { value: '', label: 'Барлығы' },
    { value: 'RECEIVED', label: 'Кезекте' },
    { value: 'PROCESSING', label: 'Өңделуде' },
    { value: 'PROCESSED', label: 'Өңделді' },
    { value: 'FAILED', label: 'Қате' },
    { value: 'IGNORED', label: 'Еленбеді' },
  ], {
    onChange: (event) => { filters.status = event.target.value; filters.offset = 0; load(); },
  });

  mount(panel, el('div', { class: 'card' },
    el('div', { class: 'toolbar' },
      statusSelect,
      el('span', { class: 'toolbar__spacer' }),
      button('Жаңарту', { size: 'sm', iconName: 'refresh', onClick: () => load() })),
    el('div', { class: 'table-wrap' },
      el('table', { class: 'table table--cards' },
        el('thead', {}, el('tr', {},
          el('th', {}, 'Қабылданды'), el('th', {}, 'Түрі'),
          el('th', {}, 'Мәртебе'), el('th', { class: 'is-numeric' }, 'Әрекет'),
          el('th', {}, 'Қате'))),
        tableBody)),
    footer));

  await load();

  async function load() {
    try {
      const events = await api.webhookEvents(filters);
      const meta = events?._meta || { total: 0, limit: filters.limit, offset: filters.offset };

      clear(tableBody);

      if (!events || events.length === 0) {
        mount(tableBody, el('tr', {}, el('td', { colspan: '5' },
          emptyState('Оқиға жоқ',
            'Green API webhook жібергенде оқиғалар осында тіркеледі. ' +
            'Қайталанған оқиғалар автоматты түрде еленбейді.'))));
        return;
      }

      for (const event of events) {
        tableBody.append(el('tr', {},
          el('td', { 'data-label': 'Қабылданды', class: 'nowrap' }, formatDateTime(event.received_at)),
          el('td', { 'data-label': 'Түрі', class: 'mono small' }, event.event_type),
          el('td', { 'data-label': 'Мәртебе' }, badge(
            { RECEIVED: 'Кезекте', PROCESSING: 'Өңделуде', PROCESSED: 'Өңделді', FAILED: 'Қате', IGNORED: 'Еленбеді' }[event.status] || event.status,
            { PROCESSED: 'success', FAILED: 'danger', IGNORED: 'neutral', PROCESSING: 'warn' }[event.status] || 'info')),
          el('td', { 'data-label': 'Әрекет', class: 'is-numeric tnum' }, String(event.attempts)),
          el('td', { 'data-label': 'Қате', class: 'small muted' }, event.error || '—')));
      }

      mount(footer, pagination({
        total: meta.total, limit: meta.limit, offset: meta.offset,
        onChange: (offset) => { filters.offset = offset; load(); },
      }));
    } catch (err) {
      mount(tableBody, el('tr', {}, el('td', { colspan: '5' }, el('div', { class: 'empty' }, err.message))));
    }
  }
}

// ----------------------------------------------------------------- account --

async function renderAccount(panel) {
  const currentInput = input({ type: 'password', autocomplete: 'current-password' });
  const newInput = input({ type: 'password', autocomplete: 'new-password' });
  const repeatInput = input({ type: 'password', autocomplete: 'new-password' });

  const saveButton = button('Құпия сөзді өзгерту', { variant: 'primary' });

  saveButton.addEventListener('click', async () => {
    if (newInput.value !== repeatInput.value) {
      notify.error('Жаңа құпия сөздер сәйкес келмейді');
      return;
    }

    saveButton.disabled = true;
    try {
      await api.changePassword(currentInput.value, newInput.value);
      notify.success('Құпия сөз өзгертілді. Қайта кіру қажет.');
      // Every session was revoked, including this one.
      setTimeout(() => window.location.reload(), 1200);
    } catch (err) {
      notify.error(err.message);
    } finally {
      saveButton.disabled = false;
    }
  });

  mount(panel,
    el('div', { class: 'grid grid--2' },
      card('Тіркелгі', {
        body: el('dl', { class: 'kv' },
          el('dt', {}, 'Email'), el('dd', {}, state.admin?.email || '—'),
          el('dt', {}, 'Аты'), el('dd', {}, state.admin?.name || '—'),
          el('dt', {}, 'Рөл'), el('dd', {}, roleLabel(state.admin?.role)),
          el('dt', {}, 'Соңғы кіру'), el('dd', {}, formatDateTime(state.admin?.last_login_at))),
      }),
      card('Құпия сөзді өзгерту', {
        body: el('div', {},
          field('Ағымдағы құпия сөз', currentInput),
          field('Жаңа құпия сөз', newInput, { hint: 'Кемінде 10 таңба' }),
          field('Жаңа құпия сөзді қайталаңыз', repeatInput),
          el('div', { class: 'alert alert--info' },
            icon('bell', 16),
            el('div', {}, 'Құпия сөзді өзгерткенде барлық құрылғылардағы сессиялар аяқталады.')),
          saveButton),
      })),
  );
}
