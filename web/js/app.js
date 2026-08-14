// Application shell: routing, navigation and session bootstrap.

import { api, ApiError, onUnauthorized } from './api.js';
import { el, mount, clear, icon, button, notify } from './ui.js';
import {
  state, on, emit, loadSession, loadSettings, clearSession,
  connectStream, refreshUnread,
} from './store.js';

import { renderLogin } from './pages/login.js';
import { renderDashboard } from './pages/dashboard.js';
import { renderChats } from './pages/chats.js';
import { renderContacts } from './pages/contacts.js';
import { renderContactDetail } from './pages/contact.js';
import { renderCampaigns } from './pages/campaigns.js';
import { renderCampaignDetail } from './pages/campaign.js';
import { renderTemplates } from './pages/templates.js';
import { renderScheduled } from './pages/scheduled.js';
import { renderTriggers } from './pages/triggers.js';
import { renderSettings } from './pages/settings.js';

const NAV = [
  { section: 'Негізгі' },
  { path: '/', label: 'Басты бет', icon: 'dashboard', render: renderDashboard, title: 'Басты бет' },
  { path: '/chats', label: 'Чаттар', icon: 'chat', render: renderChats, title: 'Чаттар', badge: 'unread', flush: true },
  { path: '/contacts', label: 'Клиенттер', icon: 'contacts', render: renderContacts, title: 'Клиенттер' },

  { section: 'Автоматтандыру' },
  { path: '/campaigns', label: 'Кампаниялар', icon: 'campaign', render: renderCampaigns, title: 'Кампаниялар' },
  { path: '/templates', label: 'Шаблондар', icon: 'template', render: renderTemplates, title: 'Шаблондар' },
  { path: '/triggers', label: 'Триггерлер', icon: 'trigger', render: renderTriggers, title: 'Триггерлер' },
  { path: '/scheduled', label: 'Жоспарланған', icon: 'clock', render: renderScheduled, title: 'Жоспарланған хабарламалар' },

  { section: 'Жүйе' },
  { path: '/settings', label: 'Баптаулар', icon: 'settings', render: renderSettings, title: 'Баптаулар' },
];

// Detail routes are not shown in the sidebar but resolve to a page.
const DETAIL_ROUTES = [
  { pattern: /^\/contacts\/([0-9a-f-]{36})$/i, render: renderContactDetail, title: 'Клиент картасы', nav: '/contacts' },
  { pattern: /^\/campaigns\/([0-9a-f-]{36})$/i, render: renderCampaignDetail, title: 'Кампания', nav: '/campaigns' },
];

const root = document.getElementById('app');
let currentCleanup = null;

// -------------------------------------------------------------- navigation --

export function navigate(path, { replace = false } = {}) {
  if (replace) {
    window.history.replaceState({}, '', path);
  } else {
    window.history.pushState({}, '', path);
  }
  renderRoute();
}

// Intercept in-app links so the SPA router handles them.
document.addEventListener('click', (event) => {
  const anchor = event.target.closest?.('a[data-link]');
  if (!anchor) return;
  const href = anchor.getAttribute('href');
  if (!href || href.startsWith('http')) return;

  event.preventDefault();
  navigate(href);
});

window.addEventListener('popstate', () => renderRoute());

function resolveRoute(path) {
  const exact = NAV.find((item) => item.path === path);
  if (exact) return { ...exact, params: [] };

  for (const route of DETAIL_ROUTES) {
    const match = path.match(route.pattern);
    if (match) return { ...route, params: match.slice(1) };
  }
  return null;
}

// ------------------------------------------------------------------ layout --

function renderShell() {
  const sidebar = el('aside', { class: 'sidebar', id: 'sidebar' },
    el('div', { class: 'sidebar__brand' },
      el('div', { class: 'sidebar__mark' }, icon('chat', 17)),
      el('div', { class: 'sidebar__name' }, 'Автоматтандыру')),
    el('nav', { class: 'sidebar__nav', id: 'sidebar-nav' }),
    el('div', { class: 'sidebar__footer' },
      el('div', { class: 'user-chip' },
        el('div', { class: 'user-chip__avatar' }, initialsFromAdmin()),
        el('div', { class: 'user-chip__meta' },
          el('div', { class: 'user-chip__name truncate' }, state.admin?.name || state.admin?.email || ''),
          el('div', { class: 'user-chip__role truncate' }, roleLabel(state.admin?.role))),
        button('', {
          iconName: 'logout', variant: 'ghost', size: 'sm',
          title: 'Шығу',
          onClick: handleLogout,
        }))),
  );

  const topbar = el('header', { class: 'topbar' },
    el('button', { class: 'burger', 'aria-label': 'Мәзір', onClick: toggleSidebar }, icon('menu', 18)),
    el('div', { class: 'topbar__title', id: 'page-title' }, ''),
    el('div', { class: 'topbar__spacer' }),
    el('div', { id: 'stream-indicator', class: 'row small subtle' }),
  );

  const main = el('main', { class: 'main' },
    topbar,
    el('div', { class: 'page', id: 'page-root' }),
  );

  mount(root, el('div', { class: 'shell' }, sidebar, main));
  root.classList.remove('app-loading');

  renderNav();
  renderStreamIndicator(state.streamConnected);
}

function renderNav() {
  const nav = document.getElementById('sidebar-nav');
  if (!nav) return;
  clear(nav);

  const path = window.location.pathname;
  const active = resolveRoute(path);
  const activePath = active?.nav || active?.path;

  for (const item of NAV) {
    if (item.section) {
      nav.append(el('div', { class: 'nav-section' }, item.section));
      continue;
    }

    const link = el('a', {
      class: `nav-link${item.path === activePath ? ' is-active' : ''}`,
      href: item.path,
      'data-link': '',
    }, icon(item.icon, 17), el('span', { class: 'truncate' }, item.label));

    if (item.badge === 'unread' && state.unreadTotal > 0) {
      link.append(el('span', { class: 'nav-link__badge' }, String(state.unreadTotal)));
    }

    link.addEventListener('click', closeSidebar);
    nav.append(link);
  }
}

function renderStreamIndicator(connected) {
  const holder = document.getElementById('stream-indicator');
  if (!holder) return;

  clear(holder);
  holder.append(
    el('span', {
      class: 'stat__dot',
      style: { background: connected ? 'var(--success)' : 'var(--text-subtle)' },
      title: connected ? 'Нақты уақыттағы байланыс белсенді' : 'Байланыс жоқ',
    }),
    el('span', { class: 'nowrap' }, connected ? 'Онлайн' : 'Офлайн'),
  );
}

function toggleSidebar() {
  const sidebar = document.getElementById('sidebar');
  if (!sidebar) return;

  const opening = !sidebar.classList.contains('is-open');
  sidebar.classList.toggle('is-open', opening);

  const existing = document.getElementById('scrim');
  if (existing) existing.remove();

  if (opening) {
    const scrim = el('div', { class: 'scrim', id: 'scrim', onClick: closeSidebar });
    document.body.append(scrim);
  }
}

function closeSidebar() {
  document.getElementById('sidebar')?.classList.remove('is-open');
  document.getElementById('scrim')?.remove();
}

function initialsFromAdmin() {
  const source = state.admin?.name || state.admin?.email || '?';
  return source.trim().slice(0, 2).toUpperCase();
}

function roleLabel(role) {
  return { OWNER: 'Иесі', ADMIN: 'Әкімші', VIEWER: 'Бақылаушы' }[role] || role || '';
}

// ------------------------------------------------------------------ render --

async function renderRoute() {
  if (!state.admin) {
    showLogin();
    return;
  }

  if (!document.getElementById('page-root')) {
    renderShell();
  }

  const path = window.location.pathname;
  const route = resolveRoute(path);

  if (!route) {
    // Unknown path: land on the dashboard rather than showing a dead end.
    navigate('/', { replace: true });
    return;
  }

  if (currentCleanup) {
    try {
      currentCleanup();
    } catch (err) {
      console.error('page cleanup failed', err);
    }
    currentCleanup = null;
  }

  renderNav();

  const titleNode = document.getElementById('page-title');
  if (titleNode) titleNode.textContent = route.title || '';

  const pageRoot = document.getElementById('page-root');
  pageRoot.className = route.flush ? 'page page--flush' : 'page';
  clear(pageRoot);

  try {
    const cleanup = await route.render(pageRoot, { params: route.params, navigate });
    if (typeof cleanup === 'function') currentCleanup = cleanup;
  } catch (err) {
    if (err instanceof ApiError && err.isAuthError) return;
    console.error('page render failed', err);
    mount(pageRoot, el('div', { class: 'alert alert--danger' },
      el('div', {},
        el('div', { class: 'alert__title' }, 'Бетті жүктеу мүмкін болмады'),
        el('div', {}, err.message || 'Белгісіз қате'))));
  }
}

function showLogin() {
  if (currentCleanup) {
    currentCleanup();
    currentCleanup = null;
  }
  root.classList.remove('app-loading');
  clear(root);
  renderLogin(root, async () => {
    await bootAuthenticated();
    navigate('/', { replace: true });
  });
}

async function handleLogout() {
  try {
    await api.logout();
  } catch {
    // Logging out locally matters more than the round trip succeeding.
  }
  clearSession();
  showLogin();
}

// ------------------------------------------------------------------- boot --

async function bootAuthenticated() {
  await loadSettings();
  connectStream();
  await refreshUnread();
  renderShell();
}

on('stream:status', renderStreamIndicator);
on('unread', renderNav);

// Any inbound message anywhere refreshes the sidebar badge, so the operator
// notices a new conversation without being on the chats page.
on('chat.updated', () => {
  if (window.location.pathname !== '/chats') refreshUnread();
});

onUnauthorized(() => {
  if (!state.admin) return;
  clearSession();
  notify.warn('Сессия аяқталды. Қайта кіріңіз.');
  showLogin();
});

async function boot() {
  try {
    await loadSession();
    await bootAuthenticated();
    await renderRoute();
  } catch (err) {
    if (err instanceof ApiError && err.isAuthError) {
      showLogin();
      return;
    }
    root.classList.remove('app-loading');
    mount(root, el('div', { class: 'login' },
      el('div', { class: 'login__card' },
        el('h1', { class: 'login__title' }, 'Қосылу мүмкін болмады'),
        el('p', { class: 'login__sub' }, err.message || 'Сервер жауап бермеді'),
        button('Қайталау', { variant: 'primary', onClick: () => window.location.reload() }))));
  }
}

boot();
