import { api } from '../api.js';
import { el, mount, clear, icon, button, badge, notify, emptyState } from '../ui.js';
import { formatDateTime, MATCH_MODE } from '../format.js';

export async function renderTriggers(root, { navigate }) {
  const holder = el('div', {});

  mount(root,
    el('div', { class: 'page-head' },
      el('div', { class: 'page-head__text' },
        el('h1', {}, 'Триггерлер'),
        el('p', { class: 'page-head__desc' },
          'Тек осы мәтіндерді жіберген клиенттер автоматтандыруға қосылады. ' +
          'Триггерді кампания бетінен қосасыз.'))),
    el('div', { class: 'alert alert--info' },
      icon('bell', 17),
      el('div', {},
        el('div', { class: 'alert__title' }, 'Қалай жұмыс істейді'),
        el('div', {},
          'Кіріс хабарлама нормаланады: регистр, артық бос орындар, тырнақша мен сызықша ' +
          'нұсқалары ескерілмейді. Бір мәтін бір ғана белсенді кампанияға тиесілі бола алады — ' +
          'бірақ әр WhatsApp нөмірінің өз тізімі бар, сондықтан бірдей сөз екі нөмірде де бола алады.'))),
    holder,
  );

  await load();

  async function load() {
    mount(holder, el('div', { class: 'card' }, el('div', { class: 'card__body' },
      el('div', { class: 'skeleton', style: { height: '40px' } }))));

    try {
      const [triggers, stats, accounts] = await Promise.all([
        api.triggers(),
        api.triggerStats().catch(() => []),
        api.whatsappInstances().catch(() => []),
      ]);

      const counts = new Map((stats || []).map((s) => [s.keyword, s.count]));
      // A trigger word only ever reaches the number it was sent to, so the
      // same word can legitimately appear twice in this table on two different
      // numbers. Naming the account is what makes that readable rather than
      // looking like a duplicate.
      const accountNames = new Map((accounts || []).map((a) => [a.id, a.name || a.instance_key]));
      const showAccounts = (accounts || []).length > 1;
      clear(holder);

      if (!triggers || triggers.length === 0) {
        holder.append(el('div', { class: 'card' }, el('div', { class: 'card__body' },
          emptyState('Триггер жоқ',
            'Кампанияны ашып, «Триггер қосу» батырмасын басыңыз.',
            button('Кампанияларға өту', { variant: 'primary', onClick: () => navigate('/campaigns') })))));
        return;
      }

      const table = el('table', { class: 'table table--cards' },
        el('thead', {}, el('tr', {},
          el('th', {}, 'Триггер мәтіні'),
          showAccounts ? el('th', {}, 'WhatsApp') : null,
          el('th', {}, 'Кампания'),
          el('th', {}, 'Режим'),
          el('th', {}, 'Мәртебе'),
          el('th', { class: 'is-numeric' }, 'Қосылған клиент'),
          el('th', {}, 'Құрылған'),
          el('th', {}))),
        el('tbody', {}, ...triggers.map((trigger) => el('tr', {},
          el('td', { 'data-label': 'Мәтін' },
            el('div', { class: 'mono', style: { maxWidth: '380px', wordBreak: 'break-word' } }, trigger.keyword)),
          showAccounts
            ? el('td', { 'data-label': 'WhatsApp' },
                badge(accountNames.get(trigger.whatsapp_instance_id) || '—', 'info'))
            : null,
          el('td', { 'data-label': 'Кампания' },
            el('a', { href: `/campaigns/${trigger.campaign_id}`, 'data-link': '' }, trigger.campaign_name)),
          el('td', { 'data-label': 'Режим' }, badge(MATCH_MODE[trigger.match_mode] || trigger.match_mode, 'neutral')),
          el('td', { 'data-label': 'Мәртебе' },
            trigger.is_active ? badge('Белсенді', 'success') : badge('Өшірулі', 'neutral')),
          el('td', { 'data-label': 'Клиент', class: 'is-numeric tnum' },
            String(counts.get(trigger.keyword) ?? 0)),
          el('td', { 'data-label': 'Құрылған', class: 'nowrap' }, formatDateTime(trigger.created_at)),
          el('td', { class: 'table__actions' },
            button('Кампания', {
              size: 'sm', iconName: 'eye',
              onClick: () => navigate(`/campaigns/${trigger.campaign_id}`),
            }))))),
      );

      holder.append(el('div', { class: 'card' }, el('div', { class: 'table-wrap' }, table)));
    } catch (err) {
      mount(holder, el('div', { class: 'alert alert--danger' }, err.message));
    }
  }
}
