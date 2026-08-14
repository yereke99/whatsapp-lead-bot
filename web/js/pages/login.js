import { api, setCsrfToken } from '../api.js';
import { el, mount, icon, button, field, input, notify } from '../ui.js';
import { state } from '../store.js';

export function renderLogin(root, onSuccess) {
  const emailInput = input({
    type: 'email',
    id: 'login-email',
    name: 'email',
    autocomplete: 'username',
    placeholder: 'admin@example.com',
    required: true,
  });

  const passwordInput = input({
    type: 'password',
    id: 'login-password',
    name: 'password',
    autocomplete: 'current-password',
    placeholder: '••••••••••',
    required: true,
  });

  const errorBox = el('div', { class: 'alert alert--danger hidden' });
  const submit = button('Кіру', { variant: 'primary', type: 'submit' });
  submit.classList.add('btn--block');

  const form = el('form', { class: 'stack' },
    field('Email', emailInput),
    field('Құпия сөз', passwordInput),
    errorBox,
    submit,
  );

  form.addEventListener('submit', async (event) => {
    event.preventDefault();

    const email = emailInput.value.trim();
    const password = passwordInput.value;

    if (!email || !password) {
      showError('Email және құпия сөзді енгізіңіз');
      return;
    }

    submit.disabled = true;
    submit.textContent = 'Тексерілуде…';
    errorBox.classList.add('hidden');

    try {
      const result = await api.login(email, password);
      setCsrfToken(result.csrf_token);
      state.admin = result.admin;
      notify.success(`Қош келдіңіз, ${result.admin.name || result.admin.email}`);
      await onSuccess();
    } catch (err) {
      showError(err.message || 'Кіру мүмкін болмады');
      passwordInput.value = '';
      passwordInput.focus();
    } finally {
      submit.disabled = false;
      submit.textContent = 'Кіру';
    }
  });

  function showError(message) {
    errorBox.textContent = message;
    errorBox.classList.remove('hidden');
  }

  mount(root, el('div', { class: 'login' },
    el('div', { class: 'login__card' },
      el('div', { class: 'login__logo' }, icon('chat', 22)),
      el('h1', { class: 'login__title' }, 'Әкімші панелі'),
      el('p', { class: 'login__sub' }, 'WhatsApp кампанияларын басқару жүйесі'),
      form)));

  setTimeout(() => emailInput.focus(), 60);
}
