// DOM construction helpers, icons, toasts and modals.
//
// Everything builds real nodes rather than assembling HTML strings, so
// user-supplied text can never be interpreted as markup.

export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);

  for (const [key, value] of Object.entries(attrs || {})) {
    if (value === null || value === undefined || value === false) continue;

    if (key === 'class') {
      node.className = value;
    } else if (key === 'dataset') {
      Object.assign(node.dataset, value);
    } else if (key === 'style' && typeof value === 'object') {
      Object.assign(node.style, value);
    } else if (key.startsWith('on') && typeof value === 'function') {
      node.addEventListener(key.slice(2).toLowerCase(), value);
    } else if (key === 'html') {
      // Only ever called with markup this module generated itself.
      node.innerHTML = value;
    } else if (value === true) {
      node.setAttribute(key, '');
    } else {
      node.setAttribute(key, String(value));
    }
  }

  appendChildren(node, children);
  return node;
}

function appendChildren(node, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
}

export function frag(...children) {
  const f = document.createDocumentFragment();
  appendChildren(f, children);
  return f;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

export function mount(node, ...children) {
  clear(node);
  appendChildren(node, children);
  return node;
}

// ------------------------------------------------------------------ icons --

const ICON_PATHS = {
  dashboard: 'M3 3h7v7H3zM14 3h7v4h-7zM14 10h7v11h-7zM3 13h7v8H3z',
  chat: 'M21 11.5a8.4 8.4 0 01-9 8.4 8.9 8.9 0 01-3.9-.9L3 20.5l1.5-4.6A8.4 8.4 0 0112 3.1a8.4 8.4 0 019 8.4z',
  contacts: 'M17 20v-2a4 4 0 00-4-4H6a4 4 0 00-4 4v2M9.5 10a4 4 0 100-8 4 4 0 000 8zM22 20v-2a4 4 0 00-3-3.9M16 3.1a4 4 0 010 7.8',
  campaign: 'M3 11l18-7v16l-18-7v-2zM7 12.5V19a1 1 0 001 1h2a1 1 0 001-1v-5',
  template: 'M4 3h16a1 1 0 011 1v16a1 1 0 01-1 1H4a1 1 0 01-1-1V4a1 1 0 011-1zM3 9h18M9 21V9',
  automation: 'M12 2v4M12 18v4M4.9 4.9l2.9 2.9M16.2 16.2l2.9 2.9M2 12h4M18 12h4M4.9 19.1l2.9-2.9M16.2 7.8l2.9-2.9',
  clock: 'M12 22a10 10 0 100-20 10 10 0 000 20zM12 6v6l4 2',
  trigger: 'M13 2L4.1 12.7a1 1 0 00.8 1.6H11l-1 7.7 8.9-10.7a1 1 0 00-.8-1.6H12l1-7.7z',
  settings: 'M12 15.5a3.5 3.5 0 100-7 3.5 3.5 0 000 7z M19.4 15a1.7 1.7 0 00.3 1.8l.1.1a2 2 0 11-2.8 2.8l-.1-.1a1.7 1.7 0 00-2.9 1.2v.2a2 2 0 01-4 0v-.1a1.7 1.7 0 00-1.1-1.6 1.7 1.7 0 00-1.9.4l-.1.1a2 2 0 11-2.8-2.8l.1-.1A1.7 1.7 0 004 15a1.7 1.7 0 00-1.5-1H2a2 2 0 010-4h.1A1.7 1.7 0 004 8.6a1.7 1.7 0 00-.4-1.9l-.1-.1a2 2 0 112.8-2.8l.1.1A1.7 1.7 0 009 4.5a1.7 1.7 0 001-1.6V2a2 2 0 014 0v.1a1.7 1.7 0 001 1.6 1.7 1.7 0 001.9-.4l.1-.1a2 2 0 112.8 2.8l-.1.1a1.7 1.7 0 00-.3 1.9v.1a1.7 1.7 0 001.5 1h.2a2 2 0 010 4h-.1a1.7 1.7 0 00-1.6 1z',
  logout: 'M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4M16 17l5-5-5-5M21 12H9',
  send: 'M22 2L11 13M22 2l-7 20-4-9-9-4 20-7z',
  attach: 'M21.4 11.05l-9.19 9.19a6 6 0 01-8.49-8.49l9.2-9.19a4 4 0 015.65 5.66l-9.2 9.19a2 2 0 01-2.83-2.83l8.49-8.48',
  search: 'M11 19a8 8 0 100-16 8 8 0 000 16zM21 21l-4.35-4.35',
  plus: 'M12 5v14M5 12h14',
  edit: 'M11 4H4a2 2 0 00-2 2v14a2 2 0 002 2h14a2 2 0 002-2v-7M18.5 2.5a2.12 2.12 0 013 3L12 15l-4 1 1-4 9.5-9.5z',
  trash: 'M3 6h18M8 6V4a1 1 0 011-1h6a1 1 0 011 1v2M19 6l-1 14a2 2 0 01-2 2H8a2 2 0 01-2-2L5 6M10 11v6M14 11v6',
  copy: 'M20 9h-9a2 2 0 00-2 2v9a2 2 0 002 2h9a2 2 0 002-2v-9a2 2 0 00-2-2zM5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1',
  play: 'M5 3l14 9-14 9V3z',
  pause: 'M6 4h4v16H6zM14 4h4v16h-4z',
  check: 'M20 6L9 17l-5-5',
  x: 'M18 6L6 18M6 6l12 12',
  menu: 'M3 12h18M3 6h18M3 18h18',
  back: 'M19 12H5M12 19l-7-7 7-7',
  download: 'M21 15v4a2 2 0 01-2 2H5a2 2 0 01-2-2v-4M7 10l5 5 5-5M12 15V3',
  refresh: 'M23 4v6h-6M1 20v-6h6M3.5 9a9 9 0 0114.9-3.4L23 10M1 14l4.6 4.4A9 9 0 0020.5 15',
  block: 'M12 22a10 10 0 100-20 10 10 0 000 20zM4.9 4.9l14.2 14.2',
  bell: 'M18 8a6 6 0 10-12 0c0 7-3 9-3 9h18s-3-2-3-9M13.7 21a2 2 0 01-3.4 0',
  file: 'M13 2H6a2 2 0 00-2 2v16a2 2 0 002 2h12a2 2 0 002-2V9l-7-7zM13 2v7h7',
  image: 'M19 3H5a2 2 0 00-2 2v14a2 2 0 002 2h14a2 2 0 002-2V5a2 2 0 00-2-2zM8.5 10a1.5 1.5 0 100-3 1.5 1.5 0 000 3zM21 15l-5-5L5 21',
  mic: 'M12 15a3 3 0 003-3V6a3 3 0 00-6 0v6a3 3 0 003 3zM19 10v2a7 7 0 01-14 0v-2M12 19v4M8 23h8',
  video: 'M23 7l-7 5 7 5V7zM14 5H3a2 2 0 00-2 2v10a2 2 0 002 2h11a2 2 0 002-2V7a2 2 0 00-2-2z',
  alert: 'M10.3 3.9L1.8 18a2 2 0 001.7 3h17a2 2 0 001.7-3L13.7 3.9a2 2 0 00-3.4 0zM12 9v4M12 17h.01',
  users: 'M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2M9 11a4 4 0 100-8 4 4 0 000 8zM23 21v-2a4 4 0 00-3-3.9M16 3.1a4 4 0 010 7.8',
  activity: 'M22 12h-4l-3 9L9 3l-3 9H2',
  inbox: 'M22 12h-6l-2 3h-4l-2-3H2M5.5 5.1L2 12v6a2 2 0 002 2h16a2 2 0 002-2v-6l-3.5-6.9A2 2 0 0016.8 4H7.2a2 2 0 00-1.7 1.1z',
  eye: 'M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8zM12 15a3 3 0 100-6 3 3 0 000 6z',
  link: 'M10 13a5 5 0 007.5.5l3-3a5 5 0 00-7-7l-1.7 1.7M14 11a5 5 0 00-7.5-.5l-3 3a5 5 0 007 7l1.7-1.7',
  up: 'M18 15l-6-6-6 6',
  down: 'M6 9l6 6 6-6',
};

export function icon(name, size = 18) {
  const path = ICON_PATHS[name];
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('width', String(size));
  svg.setAttribute('height', String(size));
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('fill', 'none');
  svg.setAttribute('stroke', 'currentColor');
  svg.setAttribute('stroke-width', '1.7');
  svg.setAttribute('stroke-linecap', 'round');
  svg.setAttribute('stroke-linejoin', 'round');
  svg.setAttribute('aria-hidden', 'true');

  if (path) {
    const p = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    p.setAttribute('d', path);
    svg.append(p);
  }
  return svg;
}

// ------------------------------------------------------------- components --

export function badge(label, tone = 'neutral') {
  return el('span', { class: `badge badge--${tone}` }, label);
}

export function button(label, { variant = '', size = '', iconName = '', onClick, type = 'button', title, disabled } = {}) {
  const classes = ['btn'];
  if (variant) classes.push(`btn--${variant}`);
  if (size) classes.push(`btn--${size}`);
  if (!label && iconName) classes.push('btn--icon');

  return el('button', {
    class: classes.join(' '),
    type,
    title: title || (label ? undefined : iconName),
    'aria-label': label || title || iconName,
    disabled: disabled || false,
    onClick,
  }, iconName ? icon(iconName, size === 'sm' ? 15 : 16) : null, label || null);
}

export function field(labelText, control, { hint, error, hintInline } = {}) {
  return el('div', { class: 'field' },
    labelText
      ? el('label', { class: 'label', for: control.id || undefined },
          labelText,
          hintInline ? el('span', { class: 'label__hint' }, hintInline) : null)
      : null,
    control,
    hint ? el('div', { class: 'field__help' }, hint) : null,
    error ? el('div', { class: 'field__error' }, error) : null,
  );
}

export function input(attrs = {}) {
  return el('input', { class: 'input', ...attrs });
}

export function textarea(attrs = {}) {
  return el('textarea', { class: 'textarea', ...attrs });
}

export function select(options, attrs = {}) {
  const node = el('select', { class: 'select', ...attrs });
  for (const option of options) {
    node.append(el('option', {
      value: option.value,
      selected: option.value === attrs.value ? true : null,
    }, option.label));
  }
  // Setting value after the options exist makes the initial selection stick.
  if (attrs.value !== undefined) node.value = attrs.value;
  return node;
}

export function checkbox(labelText, { checked = false, hint = '', onChange, name } = {}) {
  const box = el('input', { type: 'checkbox', name, checked: checked || null, onChange });
  const wrapper = el('label', { class: 'checkbox' },
    box,
    el('span', { class: 'checkbox__text' },
      labelText,
      hint ? el('span', { class: 'checkbox__hint' }, hint) : null),
  );
  wrapper.input = box;
  return wrapper;
}

export function card(titleText, { subtitle, actions, body, flush } = {}) {
  return el('div', { class: 'card' },
    titleText || actions
      ? el('div', { class: 'card__head' },
          el('div', {},
            el('div', { class: 'card__title' }, titleText),
            subtitle ? el('div', { class: 'card__sub' }, subtitle) : null),
          actions ? el('div', { class: 'card__actions' }, actions) : null)
      : null,
    el('div', { class: flush ? 'card__body card__body--flush' : 'card__body' }, body),
  );
}

export function emptyState(title, text, action) {
  return el('div', { class: 'empty' },
    el('div', { class: 'empty__icon' }, icon('inbox', 34)),
    el('div', { class: 'empty__title' }, title),
    text ? el('p', { class: 'empty__text' }, text) : null,
    action ? el('div', { class: 'mt-3' }, action) : null,
  );
}

export function skeletonRows(count = 5, columns = 4) {
  const rows = [];
  for (let i = 0; i < count; i += 1) {
    const cells = [];
    for (let c = 0; c < columns; c += 1) {
      cells.push(el('td', {}, el('div', { class: 'skeleton', style: { width: `${40 + ((i + c) % 4) * 15}%` } })));
    }
    rows.push(el('tr', {}, cells));
  }
  return rows;
}

export function alert(kind, title, text) {
  return el('div', { class: `alert alert--${kind}` },
    icon(kind === 'danger' ? 'alert' : kind === 'success' ? 'check' : 'bell', 17),
    el('div', {},
      title ? el('div', { class: 'alert__title' }, title) : null,
      text ? el('div', {}, text) : null),
  );
}

export function avatar(contact, size = '') {
  const classes = ['avatar'];
  if (size) classes.push(`avatar--${size}`);

  const name = contact?.name || contact?.push_name || '';
  const url = contact?.avatar_url;

  if (url) {
    const img = el('img', { src: url, alt: '', loading: 'lazy' });
    // A dead avatar url must not leave a broken image icon in the list.
    img.addEventListener('error', () => {
      const holder = img.parentElement;
      if (holder) {
        holder.textContent = initialsOf(name, contact?.phone);
      }
    });
    return el('div', { class: classes.join(' ') }, img);
  }

  return el('div', { class: classes.join(' ') }, initialsOf(name, contact?.phone));
}

function initialsOf(name, phone) {
  const source = (name || '').trim();
  if (source) {
    return source.split(/\s+/).slice(0, 2).map((w) => w[0].toUpperCase()).join('');
  }
  const digits = String(phone || '').replace(/\D/g, '');
  return digits.slice(-2) || '?';
}

// ----------------------------------------------------------------- toasts --

export function toast(message, { title = '', kind = 'info', timeout = 4500 } = {}) {
  const root = document.getElementById('toasts');
  if (!root) return;

  const node = el('div', { class: `toast toast--${kind}` },
    el('div', { class: 'toast__body' },
      title ? el('div', { class: 'toast__title' }, title) : null,
      el('div', { class: 'toast__text' }, message)),
    el('button', {
      class: 'btn btn--ghost btn--sm btn--icon',
      'aria-label': 'Жабу',
      onClick: () => node.remove(),
    }, icon('x', 14)),
  );

  root.append(node);
  if (timeout > 0) setTimeout(() => node.remove(), timeout);
}

export const notify = {
  success: (msg, title = 'Дайын') => toast(msg, { title, kind: 'success' }),
  error: (msg, title = 'Қате') => toast(msg, { title, kind: 'error', timeout: 7000 }),
  warn: (msg, title = 'Назар аударыңыз') => toast(msg, { title, kind: 'warn' }),
  info: (msg, title = '') => toast(msg, { title, kind: 'info' }),
};

// ----------------------------------------------------------------- modals --

export function openModal({ title, body, footer, wide = false, onClose }) {
  const root = document.getElementById('modal-root');

  const close = () => {
    document.removeEventListener('keydown', onKeyDown);
    backdrop.remove();
    if (onClose) onClose();
  };

  const onKeyDown = (event) => {
    if (event.key === 'Escape') close();
  };

  const modal = el('div', { class: wide ? 'modal modal--wide' : 'modal', role: 'dialog', 'aria-modal': 'true' },
    el('div', { class: 'modal__head' },
      el('div', { class: 'modal__title' }, title),
      el('button', { class: 'btn btn--ghost btn--sm btn--icon', 'aria-label': 'Жабу', onClick: close }, icon('x', 16))),
    el('div', { class: 'modal__body' }, body),
    footer ? el('div', { class: 'modal__foot' }, footer) : null,
  );

  const backdrop = el('div', {
    class: 'modal-backdrop',
    onClick: (event) => { if (event.target === backdrop) close(); },
  }, modal);

  document.addEventListener('keydown', onKeyDown);
  root.append(backdrop);

  // Focus the first control so keyboard users land inside the dialog.
  const focusable = modal.querySelector('input, textarea, select, button');
  if (focusable) setTimeout(() => focusable.focus(), 40);

  return { close, modal };
}

export function confirmDialog({ title, message, confirmLabel = 'Растау', danger = false }) {
  return new Promise((resolve) => {
    let settled = false;
    const finish = (value) => {
      if (settled) return;
      settled = true;
      resolve(value);
    };

    const handle = openModal({
      title,
      body: el('p', { class: 'muted' }, message),
      footer: [
        button('Болдырмау', { onClick: () => { finish(false); handle.close(); } }),
        button(confirmLabel, {
          variant: danger ? 'danger' : 'primary',
          onClick: () => { finish(true); handle.close(); },
        }),
      ],
      onClose: () => finish(false),
    });
  });
}

// pagination renders the shared footer for list views.
export function pagination({ total, limit, offset, onChange }) {
  const from = total === 0 ? 0 : offset + 1;
  const to = Math.min(offset + limit, total);

  return el('div', { class: 'pagination' },
    el('span', {}, `${from}–${to} / ${total}`),
    el('span', { class: 'pagination__spacer' }),
    button('', {
      iconName: 'back', size: 'sm',
      disabled: offset <= 0,
      title: 'Артқа',
      onClick: () => onChange(Math.max(0, offset - limit)),
    }),
    button('', {
      iconName: 'send', size: 'sm',
      disabled: offset + limit >= total,
      title: 'Алға',
      onClick: () => onChange(offset + limit),
    }),
  );
}

// linkify turns bare urls in message text into anchors without ever inserting
// raw HTML.
export function linkify(text) {
  const container = document.createDocumentFragment();
  const pattern = /(https?:\/\/[^\s<]+)/g;

  let lastIndex = 0;
  let match;
  while ((match = pattern.exec(text)) !== null) {
    if (match.index > lastIndex) {
      container.append(document.createTextNode(text.slice(lastIndex, match.index)));
    }
    container.append(el('a', {
      href: match[0],
      target: '_blank',
      rel: 'noopener noreferrer nofollow',
    }, match[0]));
    lastIndex = match.index + match[0].length;
  }

  if (lastIndex < text.length) {
    container.append(document.createTextNode(text.slice(lastIndex)));
  }
  return container;
}

// debounce delays a call until input settles, used by search boxes.
export function debounce(fn, wait = 300) {
  let timer;
  return (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), wait);
  };
}
