// Shared application state and the small event bus the pages use to react to
// session changes.

import { api, setCsrfToken } from './api.js';
import { setTimezone } from './format.js';

const listeners = new Map();

export const state = {
  admin: null,
  settings: null,
  timezone: 'Asia/Almaty',
  providerState: null,
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
  emit('session', null);
}

