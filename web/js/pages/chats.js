// Live chat console: conversation list on the left, thread and composer on
// the right, kept current by the Server-Sent Events feed.

import { api, ApiError } from '../api.js';
import {
  el, mount, clear, icon, button, avatar, badge, notify,
  emptyState, linkify, debounce, confirmDialog,
} from '../ui.js';
import {
  formatPhone, relativeTime, formatTime, dayLabel, dateInputValue,
  formatBytes, messageTypeLabel, CONTACT_STATUS,
} from '../format.js';
import { on, refreshUnread } from '../store.js';

export async function renderChats(root, { navigate }) {
  const ui = {
    chats: [],
    activeId: null,
    activeContact: null,
    messages: [],
    search: '',
    unreadOnly: false,
    loadingThread: false,
    attachment: null,
    sending: false,
  };

  const listItems = el('div', { class: 'chat-list__items', id: 'chat-list-items' });
  const searchInput = el('input', {
    class: 'input',
    type: 'search',
    placeholder: 'Іздеу: аты немесе нөмір',
    'aria-label': 'Чаттарды іздеу',
  });

  const unreadToggle = button('Оқылмаған', {
    size: 'sm',
    onClick: () => {
      ui.unreadOnly = !ui.unreadOnly;
      unreadToggle.classList.toggle('btn--primary', ui.unreadOnly);
      loadChats();
    },
  });

  searchInput.addEventListener('input', debounce(() => {
    ui.search = searchInput.value.trim();
    loadChats();
  }, 280));

  const chatList = el('aside', { class: 'chat-list' },
    el('div', { class: 'chat-list__head' },
      searchInput,
      el('div', { class: 'chat-list__filters' },
        unreadToggle,
        button('', { iconName: 'refresh', size: 'sm', title: 'Жаңарту', onClick: () => loadChats() }))),
    listItems,
  );

  const pane = el('section', { class: 'chat-pane', id: 'chat-pane' });
  const layout = el('div', { class: 'chat-layout' }, chatList, pane);

  mount(root, layout);
  renderEmptyPane();
  await loadChats();

  // ------------------------------------------------------------ chat list --

  async function loadChats() {
    try {
      const chats = await api.chats({
        search: ui.search,
        unread: ui.unreadOnly ? 'true' : '',
        limit: 60,
      });
      ui.chats = chats || [];
      renderChatList();
    } catch (err) {
      if (err instanceof ApiError && err.isAuthError) return;
      mount(listItems, el('div', { class: 'empty' }, err.message));
    }
  }

  function renderChatList() {
    clear(listItems);

    if (ui.chats.length === 0) {
      listItems.append(emptyState(
        ui.search ? 'Табылмады' : 'Чат жоқ',
        ui.search
          ? 'Іздеу шартын өзгертіп көріңіз.'
          : 'Клиент триггер жібергенде чат осында пайда болады.'));
      return;
    }

    for (const chat of ui.chats) {
      listItems.append(chatRow(chat));
    }
  }

  function chatRow(chat) {
    const classes = ['chat-item'];
    if (chat.id === ui.activeId) classes.push('is-active');
    if (chat.unread_count > 0) classes.push('is-unread');

    const row = el('button', {
      class: classes.join(' '),
      type: 'button',
      dataset: { chatId: chat.id },
      onClick: () => openChat(chat.id),
    },
      avatar(chat),
      el('div', { class: 'chat-item__body' },
        el('div', { class: 'chat-item__top' },
          el('span', { class: 'chat-item__name truncate' }, displayName(chat)),
          el('span', { class: 'chat-item__time' }, relativeTime(chat.last_activity_at))),
        el('div', { class: 'chat-item__bottom' },
          el('span', { class: 'chat-item__preview truncate' },
            (chat.last_message_direction === 'OUTGOING' ? '↩ ' : '') +
            (chat.last_message_preview || 'Хабарлама жоқ')),
          chat.unread_count > 0
            ? el('span', { class: 'chat-item__badge' }, String(chat.unread_count))
            : null)),
    );
    return row;
  }

  // --------------------------------------------------------------- thread --

  async function openChat(contactId) {
    ui.activeId = contactId;
    ui.attachment = null;
    layout.classList.add('is-thread');
    renderChatList();

    ui.loadingThread = true;
    renderPaneSkeleton();

    try {
      const [detail, messages] = await Promise.all([
        api.contact(contactId),
        api.contactMessages(contactId, { limit: 80 }),
      ]);

      ui.activeContact = detail.contact;
      ui.enrollments = detail.enrollments || [];
      ui.messages = messages || [];
      renderPane();

      if (ui.activeContact.unread_count > 0) {
        await api.markRead(contactId);
        ui.activeContact.unread_count = 0;
        const chat = ui.chats.find((c) => c.id === contactId);
        if (chat) chat.unread_count = 0;
        renderChatList();
        refreshUnread();
      }
    } catch (err) {
      if (err instanceof ApiError && err.isAuthError) return;
      mount(pane, el('div', { class: 'chat-empty' }, err.message));
    } finally {
      ui.loadingThread = false;
    }
  }

  function renderEmptyPane() {
    mount(pane, el('div', { class: 'chat-empty' },
      el('div', {},
        icon('chat', 40),
        el('h3', { class: 'mt-2' }, 'Чатты таңдаңыз'),
        el('p', { class: 'muted small mt-2' },
          'Сол жақтағы тізімнен сұхбатты ашыңыз. Жаңа хабарламалар автоматты түрде көрінеді.'))));
  }

  function renderPaneSkeleton() {
    mount(pane, el('div', { class: 'chat-thread' },
      ...Array.from({ length: 6 }, (_, i) => el('div', {
        class: `bubble bubble--${i % 2 ? 'out' : 'in'}`,
        style: { width: `${40 + (i % 3) * 15}%` },
      }, el('div', { class: 'skeleton', style: { height: '30px' } })))));
  }

  function renderPane() {
    const contact = ui.activeContact;
    if (!contact) {
      renderEmptyPane();
      return;
    }

    const status = CONTACT_STATUS[contact.status] || { label: contact.status, tone: 'neutral' };

    const header = el('div', { class: 'chat-header' },
      button('', {
        iconName: 'back', variant: 'ghost', size: 'sm',
        title: 'Артқа',
        onClick: () => {
          layout.classList.remove('is-thread');
          ui.activeId = null;
          renderChatList();
          renderEmptyPane();
        },
      }),
      avatar(contact),
      el('div', { class: 'chat-header__meta' },
        el('div', { class: 'chat-header__name truncate' }, displayName(contact)),
        el('div', { class: 'chat-header__sub' },
          el('span', {}, formatPhone(contact.phone)),
          badge(status.label, status.tone),
          contact.campaign_name ? el('span', { class: 'truncate' }, contact.campaign_name) : null)),
      el('div', { class: 'chat-header__actions' },
        button('', {
          iconName: 'refresh', variant: 'ghost', size: 'sm', title: 'Профильді жаңарту',
          onClick: refreshProfile,
        }),
        button('', {
          iconName: 'contacts', variant: 'ghost', size: 'sm', title: 'Клиент картасы',
          onClick: () => navigate(`/contacts/${contact.id}`),
        })),
    );

    // The back button only makes sense on the mobile single-pane layout.
    header.firstChild.classList.add('chat-back');

    const thread = el('div', { class: 'chat-thread', id: 'chat-thread' });
    renderMessages(thread);

    mount(pane, header, thread, buildComposer(contact));
    requestAnimationFrame(() => { thread.scrollTop = thread.scrollHeight; });
  }

  function renderMessages(thread) {
    clear(thread);

    if (ui.messages.length === 0) {
      thread.append(el('div', { class: 'chat-empty' },
        el('p', { class: 'muted' }, 'Хабарламалар тарихы бос')));
      return;
    }

    let lastDay = '';
    for (const message of ui.messages) {
      const day = dateInputValue(message.created_at);
      if (day !== lastDay) {
        lastDay = day;
        thread.append(el('div', { class: 'chat-day' }, dayLabel(message.created_at)));
      }
      thread.append(messageBubble(message));
    }
  }

  function messageBubble(message) {
    const outgoing = message.direction === 'OUTGOING';
    const failed = message.status === 'FAILED';

    const classes = ['bubble', outgoing ? 'bubble--out' : 'bubble--in'];
    if (failed) classes.push('is-failed');

    const bubble = el('div', { class: classes.join(' '), dataset: { messageId: message.id } });

    if (outgoing && (message.step_name || message.is_manual)) {
      bubble.append(el('div', { class: 'bubble__source' },
        message.is_manual
          ? `Оператор${message.admin_name ? ` · ${message.admin_name}` : ''}`
          : `Автоматтандыру · ${message.step_name}`));
    }

    const media = mediaNode(message);
    if (media) bubble.append(media);

    if (message.text) {
      bubble.append(el('div', { class: 'bubble__text' }, linkify(message.text)));
    } else if (!media) {
      bubble.append(el('div', { class: 'bubble__text muted' },
        `[${messageTypeLabel(message.type)}]`));
    }

    const meta = el('div', { class: 'bubble__meta' },
      el('span', {}, formatTime(message.created_at)));

    if (outgoing) meta.append(deliveryTicks(message));
    bubble.append(meta);

    if (failed && message.error) {
      bubble.append(el('div', { class: 'bubble__error' }, message.error));
    }

    return bubble;
  }

  function mediaNode(message) {
    const url = message.media_access_url || (message.media_url && message.media_url.startsWith('/') ? message.media_url : '');

    // An inbound attachment that has not finished downloading yet.
    if (!url && message.media_download_status === 'PENDING') {
      return el('div', { class: 'bubble__file' },
        icon('download', 18),
        el('div', {},
          el('div', { class: 'bubble__file-name' }, messageTypeLabel(message.type)),
          el('div', { class: 'bubble__file-hint' }, 'Жүктелуде…')));
    }
    if (!url) {
      if (message.media_download_status === 'FAILED') {
        return el('div', { class: 'bubble__file' },
          icon('alert', 18),
          el('div', {},
            el('div', { class: 'bubble__file-name' }, messageTypeLabel(message.type)),
            el('div', { class: 'bubble__file-hint' }, 'Файлды жүктеу мүмкін болмады')));
      }
      return null;
    }

    switch (message.type) {
      case 'IMAGE':
      case 'STICKER':
        return el('div', { class: 'bubble__media' },
          el('img', {
            src: url, alt: message.file_name || '', loading: 'lazy',
            onClick: () => window.open(url, '_blank', 'noopener'),
          }));

      case 'VIDEO':
        return el('div', { class: 'bubble__media' },
          el('video', { src: url, controls: true, preload: 'metadata' }));

      case 'VOICE':
      case 'AUDIO':
        return el('div', { class: 'bubble__media' },
          el('audio', { src: url, controls: true, preload: 'metadata' }));

      default:
        return el('a', {
          class: 'bubble__file', href: `${url}?download=true`, download: '',
        },
          icon('file', 20),
          el('div', {},
            el('div', { class: 'bubble__file-name' }, message.file_name || 'Құжат'),
            el('div', { class: 'bubble__file-hint' }, messageTypeLabel(message.type))));
    }
  }

  function deliveryTicks(message) {
    if (message.status === 'FAILED') {
      return el('span', { class: 'bubble__ticks', title: 'Жіберілмеді' }, icon('alert', 13));
    }
    if (message.status === 'PENDING') {
      return el('span', { class: 'bubble__ticks', title: 'Жіберілуде' }, icon('clock', 13));
    }

    const read = message.status === 'READ';
    const delivered = read || message.status === 'DELIVERED';

    const wrap = el('span', {
      class: `bubble__ticks${read ? ' is-read' : ''}`,
      title: read ? 'Оқылды' : delivered ? 'Жеткізілді' : 'Жіберілді',
    }, icon('check', 13));

    if (delivered) {
      const second = icon('check', 13);
      second.style.marginLeft = '-8px';
      wrap.append(second);
    }
    return wrap;
  }

  // ------------------------------------------------------------- composer --

  function buildComposer(contact) {
    // The consent rule is enforced server-side; the UI explains it rather
    // than pretending the box is usable.
    if (!contact.first_contact_at) {
      return el('div', { class: 'chat-composer' },
        el('div', { class: 'composer-blocked' },
          'Бұл байланыс бізге әлі жазбаған. Платформа ешкімге бірінші жазбайды.'));
    }
    if (contact.opted_out || contact.blocked_at) {
      return el('div', { class: 'chat-composer' },
        el('div', { class: 'composer-blocked' },
          contact.blocked_at
            ? 'Байланыс бұғатталған. Хабарлама жіберілмейді.'
            : 'Байланыс жазылымнан шыққан. Хабарлама жіберілмейді.'));
    }

    const textInput = el('textarea', {
      class: 'composer-input',
      rows: 1,
      placeholder: 'Хабарлама жазыңыз…',
      'aria-label': 'Хабарлама мәтіні',
    });

    // Grow with content up to the CSS max-height.
    textInput.addEventListener('input', () => {
      textInput.style.height = 'auto';
      textInput.style.height = `${Math.min(textInput.scrollHeight, 150)}px`;
    });

    textInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' && !event.shiftKey) {
        event.preventDefault();
        send();
      }
    });

    const fileInput = el('input', {
      type: 'file',
      class: 'hidden',
      accept: 'image/*,video/*,audio/*,.pdf,.doc,.docx,.xls,.xlsx,.ppt,.pptx,.txt,.csv',
    });

    const attachmentSlot = el('div', {});
    const sendButton = el('button', {
      class: 'composer-send', type: 'button', 'aria-label': 'Жіберу',
    }, icon('send', 17));

    sendButton.addEventListener('click', send);

    fileInput.addEventListener('change', async () => {
      const file = fileInput.files?.[0];
      fileInput.value = '';
      if (file) await attachFile(file);
    });

    const composer = el('div', { class: 'chat-composer' },
      attachmentSlot,
      el('div', { class: 'composer-row' },
        el('button', {
          class: 'composer-attach', type: 'button', 'aria-label': 'Файл тіркеу',
          onClick: () => fileInput.click(),
        }, icon('attach', 17)),
        textInput,
        sendButton),
      fileInput,
    );

    // Dropping a file straight onto the thread is the fastest path.
    composer.addEventListener('dragover', (event) => event.preventDefault());
    composer.addEventListener('drop', async (event) => {
      event.preventDefault();
      const file = event.dataTransfer?.files?.[0];
      if (file) await attachFile(file);
    });

    async function attachFile(file) {
      const kind = kindForFile(file);

      renderAttachment({ uploading: true, name: file.name, size: file.size, kind });

      try {
        const uploaded = await api.uploadMedia(file, kind);
        ui.attachment = { file: uploaded, kind: templateTypeFor(uploaded.kind, kind) };
        renderAttachment({ uploading: false });
      } catch (err) {
        ui.attachment = null;
        clear(attachmentSlot);
        notify.error(err.message || 'Файл жүктелмеді');
      }
    }

    function renderAttachment({ uploading, name, size, kind }) {
      clear(attachmentSlot);

      if (uploading) {
        attachmentSlot.append(el('div', { class: 'composer-attachment' },
          el('div', { class: 'composer-attachment__thumb' }, icon(iconForKind(kind), 18)),
          el('div', { class: 'composer-attachment__meta' },
            el('div', { class: 'truncate' }, name),
            el('div', { class: 'subtle small' }, `${formatBytes(size)} · жүктелуде…`))));
        return;
      }

      const attached = ui.attachment;
      if (!attached) return;

      const thumb = el('div', { class: 'composer-attachment__thumb' });
      if (attached.file.kind === 'IMAGE') {
        thumb.append(el('img', { src: api.mediaUrl(attached.file.id), alt: '' }));
      } else {
        thumb.append(icon(iconForKind(attached.file.kind), 18));
      }

      const typeSelect = el('select', { class: 'select', style: { width: 'auto', minHeight: '30px', padding: '2px 26px 2px 8px', fontSize: '12px' } });
      for (const option of typeOptionsFor(attached.file.kind)) {
        typeSelect.append(el('option', { value: option.value, selected: option.value === attached.kind ? true : null }, option.label));
      }
      typeSelect.value = attached.kind;
      typeSelect.addEventListener('change', () => { attached.kind = typeSelect.value; });

      attachmentSlot.append(el('div', { class: 'composer-attachment' },
        thumb,
        el('div', { class: 'composer-attachment__meta' },
          el('div', { class: 'truncate' }, attached.file.original_name),
          el('div', { class: 'subtle small' }, formatBytes(attached.file.size_bytes))),
        typeSelect,
        button('', {
          iconName: 'x', variant: 'ghost', size: 'sm', title: 'Алып тастау',
          onClick: () => { ui.attachment = null; clear(attachmentSlot); },
        })));
    }

    async function send() {
      if (ui.sending) return;

      const text = textInput.value.trim();
      const attached = ui.attachment;

      if (!text && !attached) return;

      let payload;
      if (attached) {
        const type = attached.kind;
        payload = {
          type,
          media_file_id: attached.file.id,
          file_name: attached.file.original_name,
          text: allowsCaption(type) ? text : '',
        };
        if (!allowsCaption(type) && text) {
          notify.warn('Бұл файл түрінде мәтін жіберілмейді, тек файл жіберіледі.');
        }
      } else {
        payload = { type: 'TEXT', text };
      }

      ui.sending = true;
      sendButton.disabled = true;

      try {
        const message = await api.sendMessage(ui.activeId, payload);

        // Optimistically show it; the stream event for our own send is
        // deduplicated by id below.
        appendMessage(message);

        textInput.value = '';
        textInput.style.height = 'auto';
        ui.attachment = null;
        clear(attachmentSlot);
      } catch (err) {
        notify.error(err.message || 'Хабарлама жіберілмеді');
      } finally {
        ui.sending = false;
        sendButton.disabled = false;
        textInput.focus();
      }
    }

    return composer;
  }

  async function refreshProfile() {
    try {
      const result = await api.refreshProfile(ui.activeId);
      if (ui.activeContact) ui.activeContact.avatar_url = result.avatar_url;
      notify.success('Профиль жаңартылды');
      renderPane();
      await loadChats();
    } catch (err) {
      notify.error(err.message || 'Профильді жаңарту мүмкін болмады');
    }
  }

  // -------------------------------------------------------------- realtime --

  function appendMessage(message) {
    if (!message || ui.messages.some((m) => m.id === message.id)) return;

    ui.messages.push(message);

    const thread = document.getElementById('chat-thread');
    if (!thread) return;

    // Only auto-scroll when the operator is already at the bottom, so reading
    // history is not interrupted by an arriving message.
    const atBottom = thread.scrollHeight - thread.scrollTop - thread.clientHeight < 120;

    const previous = ui.messages[ui.messages.length - 2];
    if (!previous || dateInputValue(previous.created_at) !== dateInputValue(message.created_at)) {
      thread.append(el('div', { class: 'chat-day' }, dayLabel(message.created_at)));
    }
    thread.append(messageBubble(message));

    if (atBottom) thread.scrollTop = thread.scrollHeight;
  }

  const offMessage = on('message.created', (event) => {
    const contactId = event.contact_id;
    const message = event.data;

    if (contactId === ui.activeId) {
      appendMessage(message);
      if (message.direction === 'INCOMING') {
        api.markRead(ui.activeId).catch(() => {});
      }
    }
  });

  const offStatus = on('message.status', (event) => {
    if (event.contact_id !== ui.activeId) return;

    const { external_id: externalId, status } = event.data || {};
    const message = ui.messages.find((m) => m.external_id === externalId);
    if (!message) return;

    message.status = status;

    const node = document.querySelector(`[data-message-id="${message.id}"]`);
    if (node) node.replaceWith(messageBubble(message));
  });

  const offChat = on('chat.updated', (event) => {
    const contactId = event.contact_id;
    const data = event.data || {};

    const index = ui.chats.findIndex((c) => c.id === contactId);
    if (index === -1) {
      // A conversation we have never seen: reload so it appears in order.
      loadChats();
      return;
    }

    const chat = ui.chats[index];
    chat.last_message_preview = data.preview ?? chat.last_message_preview;
    chat.last_message_direction = data.direction ?? chat.last_message_direction;
    chat.last_activity_at = data.at || new Date().toISOString();

    if (data.unread_delta && contactId !== ui.activeId) {
      chat.unread_count = (chat.unread_count || 0) + data.unread_delta;
    }

    // Move the touched conversation to the top, as any chat client would.
    ui.chats.splice(index, 1);
    ui.chats.unshift(chat);
    renderChatList();
  });

  const offContact = on('contact.updated', (event) => {
    const chat = ui.chats.find((c) => c.id === event.contact_id);
    if (chat && event.data) Object.assign(chat, event.data);

    if (event.contact_id === ui.activeId && ui.activeContact && event.data) {
      Object.assign(ui.activeContact, event.data);
    }
    renderChatList();
  });

  return () => {
    offMessage();
    offStatus();
    offChat();
    offContact();
  };
}

function displayName(contact) {
  return contact.name || contact.push_name || formatPhone(contact.phone);
}

function kindForFile(file) {
  const type = file.type || '';
  if (type.startsWith('image/')) return 'IMAGE';
  if (type.startsWith('video/')) return 'VIDEO';
  if (type.startsWith('audio/')) return 'VOICE';
  return 'DOCUMENT';
}

function iconForKind(kind) {
  return { IMAGE: 'image', VIDEO: 'video', AUDIO: 'mic', VOICE: 'mic' }[kind] || 'file';
}

// typeOptionsFor lets the operator decide how an uploaded file is delivered,
// most importantly audio as a voice note versus a music file.
function typeOptionsFor(mediaKind) {
  switch (mediaKind) {
    case 'IMAGE':
      return [
        { value: 'IMAGE_WITH_CAPTION', label: 'Сурет + мәтін' },
        { value: 'IMAGE', label: 'Тек сурет' },
      ];
    case 'VIDEO':
      return [
        { value: 'VIDEO_WITH_CAPTION', label: 'Бейне + мәтін' },
        { value: 'VIDEO', label: 'Тек бейне' },
      ];
    case 'VOICE':
      return [
        { value: 'VOICE', label: 'Дауыстық хабарлама' },
        { value: 'AUDIO', label: 'Аудио файл' },
      ];
    case 'AUDIO':
      return [
        { value: 'AUDIO', label: 'Аудио файл' },
        { value: 'VOICE', label: 'Дауыстық хабарлама' },
      ];
    default:
      return [{ value: 'DOCUMENT', label: 'Құжат' }];
  }
}

function templateTypeFor(uploadedKind, requestedKind) {
  return typeOptionsFor(uploadedKind || requestedKind)[0].value;
}

function allowsCaption(type) {
  return ['TEXT', 'IMAGE_WITH_CAPTION', 'VIDEO_WITH_CAPTION', 'DOCUMENT'].includes(type);
}
