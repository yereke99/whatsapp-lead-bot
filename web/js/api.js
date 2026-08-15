// Thin API client. Every call goes through request() so authentication,
// CSRF and error shapes are handled in exactly one place.

export class ApiError extends Error {
  constructor(status, code, message, details) {
    super(message || 'Сұраныс орындалмады');
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
  }

  get isAuthError() {
    return this.status === 401;
  }
}

function readCookie(name) {
  const prefix = `${name}=`;
  for (const part of document.cookie.split(';')) {
    const trimmed = part.trim();
    if (trimmed.startsWith(prefix)) {
      return decodeURIComponent(trimmed.slice(prefix.length));
    }
  }
  return '';
}

// Listeners fired when the server reports the session is gone, so the app can
// bounce the user to the login screen from anywhere.
const unauthorizedHandlers = new Set();

export function onUnauthorized(handler) {
  unauthorizedHandlers.add(handler);
  return () => unauthorizedHandlers.delete(handler);
}

let csrfToken = '';

export function setCsrfToken(token) {
  csrfToken = token || '';
}

function currentCsrf() {
  // The cookie is authoritative after a reload; the in-memory copy covers the
  // window between logging in and the cookie being readable.
  return csrfToken || readCookie('wa_csrf');
}

async function request(method, path, { body, query, raw, signal } = {}) {
  let url = path;
  if (query) {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(query)) {
      if (value === undefined || value === null || value === '') continue;
      params.set(key, String(value));
    }
    const qs = params.toString();
    if (qs) url += `?${qs}`;
  }

  const headers = {};
  const options = { method, headers, credentials: 'same-origin', signal };

  if (body instanceof FormData) {
    // Let the browser set the multipart boundary.
    options.body = body;
  } else if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    options.body = JSON.stringify(body);
  }

  if (method !== 'GET' && method !== 'HEAD') {
    headers['X-CSRF-Token'] = currentCsrf();
  }

  let response;
  try {
    response = await fetch(url, options);
  } catch (err) {
    if (err.name === 'AbortError') throw err;
    throw new ApiError(0, 'network_error', 'Серверге қосылу мүмкін болмады');
  }

  if (response.status === 401) {
    for (const handler of unauthorizedHandlers) handler();
    throw new ApiError(401, 'unauthorized', 'Сессия аяқталды, қайта кіріңіз');
  }

  if (raw) {
    if (!response.ok) {
      throw new ApiError(response.status, 'request_failed', 'Сұраныс орындалмады');
    }
    return response;
  }

  if (response.status === 204) return null;

  let payload = null;
  const contentType = response.headers.get('Content-Type') || '';
  if (contentType.includes('application/json')) {
    try {
      payload = await response.json();
    } catch {
      payload = null;
    }
  }

  if (!response.ok) {
    const err = payload && payload.error;
    throw new ApiError(
      response.status,
      err?.code || 'request_failed',
      err?.message || `Қате ${response.status}`,
      err?.details,
    );
  }

  // Envelope responses carry data/meta; unwrap but keep meta reachable.
  if (payload && typeof payload === 'object' && 'data' in payload) {
    const data = payload.data;
    if (payload.meta && data && typeof data === 'object') {
      Object.defineProperty(data, '_meta', { value: payload.meta, enumerable: false });
    }
    return data;
  }
  return payload;
}

const get = (path, query, opts) => request('GET', path, { query, ...opts });
const post = (path, body, opts) => request('POST', path, { body, ...opts });
const put = (path, body) => request('PUT', path, { body });
const del = (path) => request('DELETE', path, {});

export const api = {
  // auth
  login: (email, password) => post('/api/auth/login', { email, password }),
  logout: () => post('/api/auth/logout'),
  me: () => get('/api/me'),
  changePassword: (current_password, new_password) =>
    post('/api/me/password', { current_password, new_password }),

  // dashboard & analytics
  dashboard: () => get('/api/dashboard'),
  contactSeries: (days) => get('/api/analytics/contacts', { days }),
  messageSeries: (days) => get('/api/analytics/messages', { days }),
  deliveryBreakdown: (days) => get('/api/analytics/delivery', { days }),
  campaignStats: () => get('/api/analytics/campaigns'),
  triggerStats: () => get('/api/analytics/triggers'),

  // contacts
  contacts: (query) => get('/api/contacts', query),
  contact: (id) => get(`/api/contacts/${id}`),
  updateContact: (id, body) => put(`/api/contacts/${id}`, body),
  deleteContact: (id) => del(`/api/contacts/${id}`),
  contactMessages: (id, query) => get(`/api/contacts/${id}/messages`, query),
  blockContact: (id, blocked) => post(`/api/contacts/${id}/block`, { blocked }),
  unsubscribeContact: (id, opted_out) => post(`/api/contacts/${id}/unsubscribe`, { opted_out }),
  bulkAction: (body) => post('/api/contacts/bulk', body),

  // campaigns
  campaigns: (query) => get('/api/campaigns', query),
  campaign: (id) => get(`/api/campaigns/${id}`),
  createCampaign: (body) => post('/api/campaigns', body),
  updateCampaign: (id, body) => put(`/api/campaigns/${id}`, body),
  deleteCampaign: (id) => del(`/api/campaigns/${id}`),
  setCampaignStatus: (id, status) => post(`/api/campaigns/${id}/status`, { status }),
  duplicateCampaign: (id) => post(`/api/campaigns/${id}/duplicate`),
  campaignPreview: (id) => get(`/api/campaigns/${id}/preview`),

  // steps
  steps: (campaignId) => get(`/api/campaigns/${campaignId}/steps`),
  createStep: (campaignId, body) => post(`/api/campaigns/${campaignId}/steps`, body),
  updateStep: (campaignId, stepId, body) => put(`/api/campaigns/${campaignId}/steps/${stepId}`, body),
  deleteStep: (campaignId, stepId) => del(`/api/campaigns/${campaignId}/steps/${stepId}`),
  reorderSteps: (campaignId, step_ids) => post(`/api/campaigns/${campaignId}/steps/reorder`, { step_ids }),

  // triggers
  triggers: () => get('/api/triggers'),
  createTrigger: (campaignId, body) => post(`/api/campaigns/${campaignId}/triggers`, body),
  updateTrigger: (id, body) => put(`/api/triggers/${id}`, body),
  deleteTrigger: (id) => del(`/api/triggers/${id}`),

  // templates
  templates: (query) => get('/api/templates', query),
  template: (id) => get(`/api/templates/${id}`),
  createTemplate: (body) => post('/api/templates', body),
  updateTemplate: (id, body) => put(`/api/templates/${id}`, body),
  deleteTemplate: (id) => del(`/api/templates/${id}`),
  duplicateTemplate: (id) => post(`/api/templates/${id}/duplicate`),
  templatePreview: (id, query) => get(`/api/templates/${id}/preview`, query),
  templateVersions: (id) => get(`/api/templates/${id}/versions`),
  templateVariables: () => get('/api/templates/variables'),

  // media
  media: (query) => get('/api/media', query),
  uploadMedia: (file, kind, onProgress) => uploadWithProgress(file, kind, onProgress),
  deleteMedia: (id) => del(`/api/media/${id}`),
  mediaUrl: (id) => `/api/media/${id}/content`,

  // scheduler
  scheduled: (query) => get('/api/scheduled-messages', query),
  retryJob: (id) => post(`/api/scheduled-messages/${id}/retry`),
  cancelJob: (id) => post(`/api/scheduled-messages/${id}/cancel`),

  // system
  auditLogs: (query) => get('/api/audit-logs', query),
  webhookEvents: (query) => get('/api/webhook-events', query),
  providerState: () => get('/api/system/provider'),
  systemSettings: () => get('/api/system/settings'),

  // admins
  admins: () => get('/api/admins'),
  createAdmin: (body) => post('/api/admins', body),
  updateAdmin: (id, body) => put(`/api/admins/${id}`, body),
  deleteAdmin: (id) => del(`/api/admins/${id}`),

  exportContactsUrl: (query) => {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(query || {})) {
      if (value === undefined || value === null || value === '') continue;
      params.set(key, String(value));
    }
    const qs = params.toString();
    return `/api/exports/contacts${qs ? `?${qs}` : ''}`;
  },
};

// uploadWithProgress uses XMLHttpRequest because fetch cannot report upload
// progress, and a 60 MB video upload needs a progress bar.
function uploadWithProgress(file, kind, onProgress) {
  return new Promise((resolve, reject) => {
    const form = new FormData();
    form.append('file', file);
    if (kind) form.append('kind', kind);

    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/media/upload');
    xhr.withCredentials = true;
    xhr.setRequestHeader('X-CSRF-Token', currentCsrf());

    if (onProgress) {
      xhr.upload.addEventListener('progress', (event) => {
        if (event.lengthComputable) {
          onProgress(Math.round((event.loaded / event.total) * 100));
        }
      });
    }

    xhr.addEventListener('load', () => {
      let payload = null;
      try {
        payload = JSON.parse(xhr.responseText);
      } catch {
        payload = null;
      }

      if (xhr.status === 401) {
        for (const handler of unauthorizedHandlers) handler();
        reject(new ApiError(401, 'unauthorized', 'Сессия аяқталды'));
        return;
      }
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve(payload?.data ?? payload);
        return;
      }

      const err = payload?.error;
      reject(new ApiError(xhr.status, err?.code || 'upload_failed',
        err?.message || 'Файл жүктелмеді', err?.details));
    });

    xhr.addEventListener('error', () => {
      reject(new ApiError(0, 'network_error', 'Файлды жүктеу кезінде байланыс үзілді'));
    });

    xhr.send(form);
  });
}
