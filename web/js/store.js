// Shared application state and the Server-Sent Events connection that keeps
// the dashboard live.

import { api, setCsrfToken } from './api.js';
import { setTimezone } from './format.js';

const listeners = new Map();

export const state = {
  admin: null,
  settings: null,
  timezone: 'Asia/Almaty',
  unreadTotal: 0,
  providerState: null,
  streamConnected: false,
};

// on subscribes to an application event; returns an unsubscribe function.
export function on(event, handler) {
  if (!listeners.has(event)) listeners.set(event, new Set());
  listeners.get(event).add(handler);
  return () => listeners.get(event)?.delete(handler);
}

export function emit(event, payload) {
  const handlers = listeners.get(event);
  if (!handlers) return;
  for (const handler of [...handlers]) {
    try {
      handler(payload);
    } catch (err) {
      console.error(`handler for ${event} failed`, err);
    }
  }
}

export async function loadSession() {
  const session = await api.me();
  state.admin = session.admin;
  state.timezone = session.timezone || state.timezone;
  setCsrfToken(session.csrf_token);
  setTimezone(state.timezone);
  emit('session', state.admin);
  return session;
}

export async function loadSettings() {
  try {
    state.settings = await api.systemSettings();
    if (state.settings?.timezone) {
      state.timezone = state.settings.timezone;
      setTimezone(state.timezone);
    }
  } catch {
    state.settings = null;
  }
  return state.settings;
}

export function clearSession() {
  state.admin = null;
  state.settings = null;
  setCsrfToken('');
  disconnectStream();
  emit('session', null);
}

// ------------------------------------------------------------ event stream --

let source = null;
let reconnectTimer = null;
let reconnectDelay = 1000;

// connectStream opens the SSE feed. EventSource reconnects on its own, but a
// server restart closes with an error, so an explicit backoff is kept for the
// cases the browser gives up on.
export function connectStream() {
  if (source) return;

  source = new EventSource('/api/stream', { withCredentials: true });

  source.addEventListener('open', () => {
    state.streamConnected = true;
    reconnectDelay = 1000;
    emit('stream:status', true);
  });

  source.addEventListener('error', () => {
    state.streamConnected = false;
    emit('stream:status', false);

    // readyState CLOSED means the browser will not retry by itself.
    if (source && source.readyState === EventSource.CLOSED) {
      disconnectStream();
      scheduleReconnect();
    }
  });

  for (const type of [
    'message.created',
    'message.status',
    'chat.updated',
    'contact.updated',
    'job.updated',
    'campaign.updated',
    'provider.state',
  ]) {
    source.addEventListener(type, (event) => {
      let payload = null;
      try {
        payload = JSON.parse(event.data);
      } catch {
        return;
      }
      emit(type, payload);
      emit('stream:any', payload);
    });
  }
}

function scheduleReconnect() {
  if (reconnectTimer) return;
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    // Cap the delay so a long outage does not leave the dashboard silent for
    // minutes after the server comes back.
    reconnectDelay = Math.min(reconnectDelay * 2, 30000);
    connectStream();
  }, reconnectDelay);
}

export function disconnectStream() {
  if (source) {
    source.close();
    source = null;
  }
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  state.streamConnected = false;
}

// ------------------------------------------------------------ unread count --

export async function refreshUnread() {
  try {
    const chats = await api.chats({ unread: true, limit: 1 });
    state.unreadTotal = chats?._meta?.total ?? 0;
    emit('unread', state.unreadTotal);
  } catch {
    // A failed badge refresh is not worth surfacing to the operator.
  }
}
