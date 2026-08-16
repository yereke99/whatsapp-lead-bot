import { api } from '../api.js';
import {
  el, mount, clear, icon, button, badge, notify, emptyState,
  openModal, field, input, textarea, select, checkbox, confirmDialog,
  debounce, linkify,
} from '../ui.js';
import { formatDateTime, formatBytes, formatDuration, TEMPLATE_TYPE } from '../format.js';

const TYPE_MEDIA_KIND = {
  TEXT: null,
  IMAGE: 'IMAGE',
  IMAGE_WITH_CAPTION: 'IMAGE',
  VIDEO: 'VIDEO',
  VIDEO_WITH_CAPTION: 'VIDEO',
  AUDIO: 'AUDIO',
  VOICE: 'VOICE',
  DOCUMENT: 'DOCUMENT',
};

const CAPTION_TYPES = new Set(['TEXT', 'IMAGE_WITH_CAPTION', 'VIDEO_WITH_CAPTION', 'DOCUMENT']);

export async function renderTemplates(root) {
  let variables = [];
  const listHolder = el('div', { class: 'stack' });

  const searchInput = input({ type: 'search', class: 'input toolbar__search', placeholder: 'Атауы немесе мәтіні' });
  const typeFilter = select([
    { value: '', label: 'Барлық түрі' },
    ...Object.entries(TEMPLATE_TYPE).map(([value, label]) => ({ value, label })),
  ]);

  let filters = { search: '', type: '' };

  searchInput.addEventListener('input', debounce(() => {
    filters.search = searchInput.value.trim();
    load();
  }, 280));

  typeFilter.addEventListener('change', () => {
    filters.type = typeFilter.value;
    load();
  });

  mount(root,
    el('div', { class: 'page-head' },
      el('div', { class: 'page-head__text' },
        el('h1', {}, 'Шаблондар'),
        el('p', { class: 'page-head__desc' },
          'Мәтін, сурет, бейне, аудио және дауыстық хабарламалар. Кампаниялар арасында қайта қолданылады.')),
      el('div', { class: 'page-head__actions' },
        button('Жаңа шаблон', { iconName: 'plus', variant: 'primary', onClick: () => openForm(null) }))),
    // The one thing worth saying on this page: a template has no time of its
    // own. Operators otherwise look for a date field here and do not find one.
    el('div', { class: 'alert alert--info' },
      icon('clock', 16),
      el('div', {},
        el('div', { class: 'alert__title' }, 'Шаблонда уақыт болмайды'),
        el('div', { class: 'small' },
          'Мұнда тек хабарламаның мазмұны сақталады. Қашан жіберілетінін кампанияның '
          + '«Хабарламалар кезегі» бөлімінде белгілейсіз — сол жерде әр шаблонға күн мен уақыт '
          + 'немесе триггерден кейінгі кідіріс қойылады. Сондықтан бір шаблонды бірнеше '
          + 'кампанияда әртүрлі уақытта қолдануға болады.'))),
    el('div', { class: 'card' },
      el('div', { class: 'toolbar' }, searchInput, typeFilter),
      el('div', { class: 'card__body' }, listHolder)),
  );

  try {
    variables = await api.templateVariables();
  } catch {
    variables = [];
  }

  await load();

  async function load() {
    mount(listHolder, el('div', { class: 'skeleton', style: { height: '60px' } }));

    try {
      const templates = await api.templates(filters);
      clear(listHolder);

      if (!templates || templates.length === 0) {
        listHolder.append(emptyState('Шаблон жоқ',
          'Хабарлама мәтінін немесе медиа файлын осында сақтаңыз, кейін кампания қадамдарында қолданасыз.',
          button('Жаңа шаблон', { variant: 'primary', iconName: 'plus', onClick: () => openForm(null) })));
        return;
      }

      const table = el('table', { class: 'table table--cards' },
        el('thead', {}, el('tr', {},
          el('th', {}, 'Атауы'),
          el('th', {}, 'Түрі'),
          el('th', {}, 'Мазмұны'),
          el('th', { class: 'is-numeric' }, 'Нұсқа'),
          el('th', { class: 'is-numeric' }, 'Қадамдар'),
          el('th', {}, 'Жаңартылды'),
          el('th', {}))),
        el('tbody', {}, ...templates.map(templateRow)),
      );

      listHolder.append(el('div', { class: 'table-wrap' }, table));
    } catch (err) {
      mount(listHolder, el('div', { class: 'alert alert--danger' }, err.message));
    }
  }

  function templateRow(template) {
    return el('tr', {},
      el('td', { 'data-label': 'Атауы' },
        el('div', { class: 'cell-primary' }, template.name),
        template.archived_at ? badge('Мұрағат', 'neutral') : null,
        template.description ? el('div', { class: 'cell-secondary truncate' }, template.description) : null),
      el('td', { 'data-label': 'Түрі' }, badge(TEMPLATE_TYPE[template.type] || template.type, typeTone(template.type))),
      el('td', { 'data-label': 'Мазмұны' },
        el('div', { class: 'cell-secondary', style: { maxWidth: '340px' } },
          template.body
            ? truncate(template.body, 110)
            : template.media
              ? `${template.media.original_name} · ${formatBytes(template.media.size_bytes)}`
              : '—')),
      el('td', { 'data-label': 'Нұсқа', class: 'is-numeric tnum' }, `v${template.version}`),
      el('td', { 'data-label': 'Қадамдар', class: 'is-numeric tnum' }, String(template.used_by_steps)),
      el('td', { 'data-label': 'Жаңартылды', class: 'nowrap' }, formatDateTime(template.updated_at)),
      el('td', { class: 'table__actions' },
        button('', { iconName: 'eye', size: 'sm', title: 'Қарау', onClick: () => openPreview(template) }),
        button('', { iconName: 'edit', size: 'sm', title: 'Өңдеу', onClick: () => openForm(template) }),
        button('', { iconName: 'copy', size: 'sm', title: 'Көшіру', onClick: () => duplicate(template) }),
        button('', { iconName: 'trash', size: 'sm', title: 'Жою', onClick: () => remove(template) })),
    );
  }

  // ------------------------------------------------------------------ form --

  function openForm(template) {
    const isEdit = Boolean(template);

    let mediaFile = template?.media || null;

    const nameInput = input({ value: template?.name || '', placeholder: 'Шаблон атауы' });
    const descInput = input({ value: template?.description || '', placeholder: 'Ішкі сипаттама' });

    const typeSelect = select(
      Object.entries(TEMPLATE_TYPE).map(([value, label]) => ({ value, label })),
      { value: template?.type || 'TEXT' },
    );

    const bodyInput = textarea({ class: 'textarea textarea--tall', placeholder: 'Хабарлама мәтіні…' });
    bodyInput.value = template?.body || '';

    const linkPreviewBox = checkbox('Сілтеме алдын ала қарауын көрсету', {
      checked: template?.link_preview ?? true,
    });

    const mediaSlot = el('div', {});
    const bodyField = field('Мәтін', bodyInput, {
      hint: 'Айнымалыларды қосу үшін төмендегі белгілерді басыңыз',
    });

    const variableChips = el('div', { class: 'chips mt-2' },
      ...variables.map((variable) => {
        const chip = el('button', {
          type: 'button', class: 'var-chip', title: variable.description,
        }, `{{${variable.key}}}`);

        chip.addEventListener('click', () => insertAtCursor(bodyInput, `{{${variable.key}}}`));
        return chip;
      }));

    bodyField.append(variableChips);

    const fileInput = el('input', { type: 'file', class: 'hidden' });
    const dropZone = el('div', { class: 'drop-zone' },
      icon('attach', 22),
      el('div', { class: 'mt-2' }, 'Файлды осында сүйреңіз немесе таңдау үшін басыңыз'),
      el('div', { class: 'small subtle mt-2' }, 'Сурет, бейне, аудио немесе құжат'));

    dropZone.addEventListener('click', () => fileInput.click());
    dropZone.addEventListener('dragover', (event) => {
      event.preventDefault();
      dropZone.classList.add('is-over');
    });
    dropZone.addEventListener('dragleave', () => dropZone.classList.remove('is-over'));
    dropZone.addEventListener('drop', (event) => {
      event.preventDefault();
      dropZone.classList.remove('is-over');
      const file = event.dataTransfer?.files?.[0];
      if (file) upload(file);
    });

    fileInput.addEventListener('change', () => {
      const file = fileInput.files?.[0];
      fileInput.value = '';
      if (file) upload(file);
    });

    async function upload(file) {
      const kind = TYPE_MEDIA_KIND[typeSelect.value];
      mount(mediaSlot, el('div', { class: 'drop-zone' },
        el('div', { class: 'skeleton', style: { height: '14px', width: '60%', margin: '0 auto' } }),
        el('div', { class: 'small subtle mt-2' }, `${file.name} жүктелуде…`)));

      try {
        mediaFile = await api.uploadMedia(file, kind);
        notify.success(kind === 'VOICE' && mediaFile.mime_type.includes('ogg')
          ? 'Аудио WhatsApp дауыстық форматына түрлендірілді'
          : 'Файл жүктелді');
        renderMediaSlot();
      } catch (err) {
        mediaFile = null;
        notify.error(err.message);
        renderMediaSlot();
      }
    }

    function renderMediaSlot() {
      clear(mediaSlot);

      const type = typeSelect.value;
      const needsMedia = Boolean(TYPE_MEDIA_KIND[type]);

      bodyField.classList.toggle('hidden', !CAPTION_TYPES.has(type));
      linkPreviewBox.classList.toggle('hidden', type !== 'TEXT');

      if (!needsMedia) return;

      if (!mediaFile) {
        mediaSlot.append(field(`Файл (${TEMPLATE_TYPE[type]})`, dropZone, {
          hint: type === 'VOICE'
            ? 'MP3, WAV, M4A немесе OGG жүктеңіз — жүйе оны WhatsApp дауыстық хабарламасына түрлендіреді'
            : undefined,
        }));
        return;
      }

      const preview = el('div', {});
      const url = api.mediaUrl(mediaFile.id);

      if (mediaFile.kind === 'IMAGE') {
        preview.append(el('img', {
          src: url, alt: '',
          style: { maxWidth: '100%', maxHeight: '220px', borderRadius: 'var(--radius)', display: 'block' },
        }));
      } else if (mediaFile.kind === 'VIDEO') {
        preview.append(el('video', { src: url, controls: true, style: { maxWidth: '100%', borderRadius: 'var(--radius)' } }));
      } else if (mediaFile.kind === 'VOICE' || mediaFile.kind === 'AUDIO') {
        preview.append(el('audio', { src: url, controls: true, class: 'audio-preview' }));
      }

      mediaSlot.append(field(`Файл (${TEMPLATE_TYPE[type]})`,
        el('div', { class: 'card', style: { padding: '12px' } },
          el('div', { class: 'row row--between mb-2' },
            el('div', { style: { minWidth: 0 } },
              el('div', { class: 'cell-primary truncate' }, mediaFile.original_name),
              el('div', { class: 'small subtle' },
                `${formatBytes(mediaFile.size_bytes)} · ${mediaFile.mime_type}` +
                (mediaFile.duration_ms ? ` · ${formatDuration(mediaFile.duration_ms)}` : ''))),
            button('Ауыстыру', { size: 'sm', onClick: () => fileInput.click() })),
          preview)));
    }

    typeSelect.addEventListener('change', () => {
      const wantKind = TYPE_MEDIA_KIND[typeSelect.value];
      // A file of the wrong family cannot be reused for the new type.
      if (mediaFile && wantKind && mediaFile.kind !== wantKind) {
        const audioSwap = (mediaFile.kind === 'AUDIO' && wantKind === 'VOICE')
          || (mediaFile.kind === 'VOICE' && wantKind === 'AUDIO');
        if (!audioSwap) mediaFile = null;
      }
      if (!wantKind) mediaFile = null;
      renderMediaSlot();
    });

    renderMediaSlot();

    const errorBox = el('div', { class: 'alert alert--danger hidden' });
    const saveButton = button(isEdit ? 'Сақтау' : 'Құру', { variant: 'primary' });

    const handle = openModal({
      title: isEdit ? `Шаблонды өңдеу · v${template.version}` : 'Жаңа шаблон',
      wide: true,
      body: el('div', {},
        errorBox,
        el('div', { class: 'form-grid' },
          field('Атауы', nameInput),
          field('Түрі', typeSelect)),
        field('Сипаттама', descInput),
        mediaSlot,
        bodyField,
        el('div', { class: 'field' }, linkPreviewBox),
        isEdit
          ? el('div', { class: 'alert alert--info' },
              icon('bell', 16),
              el('div', {},
                el('div', { class: 'alert__title' }, 'Өзгертулер қалай қолданылады'),
                el('div', {}, 'Шаблон мазмұны жіберер алдында оқылады. Сақтағаннан кейін ' +
                  'әлі жіберілмеген барлық хабарлама жаңа нұсқамен кетеді, ал жіберілгендері өзгермейді.')))
          : null),
      footer: [button('Болдырмау', { onClick: () => handle.close() }), saveButton],
    });

    saveButton.addEventListener('click', async () => {
      errorBox.classList.add('hidden');

      const type = typeSelect.value;
      const payload = {
        name: nameInput.value.trim(),
        description: descInput.value.trim(),
        type,
        body: CAPTION_TYPES.has(type) ? bodyInput.value.trim() : '',
        media_file_id: TYPE_MEDIA_KIND[type] && mediaFile ? mediaFile.id : '',
        file_name: mediaFile?.original_name || '',
        link_preview: linkPreviewBox.input.checked,
      };

      saveButton.disabled = true;
      try {
        if (isEdit) await api.updateTemplate(template.id, payload);
        else await api.createTemplate(payload);

        notify.success('Сақталды');
        handle.close();
        await load();
      } catch (err) {
        const details = Array.isArray(err.details)
          ? err.details.map((d) => d.message).join('; ')
          : '';
        errorBox.textContent = details || err.message;
        errorBox.classList.remove('hidden');
      } finally {
        saveButton.disabled = false;
      }
    });
  }

  // --------------------------------------------------------------- preview --

  async function openPreview(template) {
    let preview = null;
    let versions = [];

    try {
      [preview, versions] = await Promise.all([
        api.templatePreview(template.id),
        api.templateVersions(template.id).catch(() => []),
      ]);
    } catch (err) {
      notify.error(err.message);
      return;
    }

    const bubble = el('div', { class: 'bubble bubble--out' });

    if (preview.media_url) {
      if (preview.media_kind === 'IMAGE') {
        bubble.append(el('div', { class: 'bubble__media' }, el('img', { src: preview.media_url, alt: '' })));
      } else if (preview.media_kind === 'VIDEO') {
        bubble.append(el('div', { class: 'bubble__media' }, el('video', { src: preview.media_url, controls: true })));
      } else if (preview.media_kind === 'VOICE' || preview.media_kind === 'AUDIO') {
        bubble.append(el('div', { class: 'bubble__media' }, el('audio', { src: preview.media_url, controls: true })));
      } else {
        bubble.append(el('div', { class: 'bubble__file' },
          icon('file', 18), el('div', { class: 'bubble__file-name' }, preview.media_name || 'Құжат')));
      }
    }

    if (preview.rendered_text) {
      bubble.append(el('div', { class: 'bubble__text' }, linkify(preview.rendered_text)));
    }
    bubble.append(el('div', { class: 'bubble__meta' }, el('span', {}, '21:00'), icon('check', 13)));

    const handle = openModal({
      title: `${template.name} · алдын ала қарау`,
      wide: true,
      body: el('div', {},
        el('p', { class: 'muted small mb-3' },
          'Айнымалылар үлгі мәндермен толтырылған. Нақты жіберілгенде клиенттің деректері қойылады.'),
        el('div', { class: 'preview-phone' }, bubble),
        preview.unknown_variables?.length
          ? el('div', { class: 'alert alert--warn mt-3' },
              icon('alert', 16),
              el('div', {},
                el('div', { class: 'alert__title' }, 'Белгісіз айнымалылар'),
                el('div', {}, preview.unknown_variables.map((v) => `{{${v}}}`).join(', ') +
                  ' — бұлар жіберер алдында алынып тасталады.')))
          : null,
        versions.length > 1
          ? el('div', { class: 'mt-3' },
              el('div', { class: 'label' }, 'Нұсқалар тарихы'),
              el('div', { class: 'table-wrap' },
                el('table', { class: 'table' },
                  el('thead', {}, el('tr', {},
                    el('th', {}, 'Нұсқа'), el('th', {}, 'Түрі'),
                    el('th', {}, 'Автор'), el('th', {}, 'Күні'))),
                  el('tbody', {}, ...versions.map((version) => el('tr', {},
                    el('td', {}, `v${version.version}`),
                    el('td', {}, TEMPLATE_TYPE[version.type] || version.type),
                    el('td', {}, version.author || '—'),
                    el('td', {}, formatDateTime(version.created_at))))))))
          : null),
      footer: [button('Жабу', { variant: 'primary', onClick: () => handle.close() })],
    });
  }

  async function duplicate(template) {
    try {
      await api.duplicateTemplate(template.id);
      notify.success('Көшірме құрылды');
      await load();
    } catch (err) {
      notify.error(err.message);
    }
  }

  async function remove(template) {
    const inUse = template.used_by_steps > 0;
    const confirmed = await confirmDialog({
      title: 'Шаблонды жою',
      message: inUse
        ? `Бұл шаблон ${template.used_by_steps} қадамда қолданылуда. Ол мұрағатқа жіберіледі, бірақ бар қадамдар жұмысын жалғастырады.`
        : 'Шаблон толығымен жойылады.',
      confirmLabel: inUse ? 'Мұрағаттау' : 'Жою',
      danger: true,
    });
    if (!confirmed) return;

    try {
      const result = await api.deleteTemplate(template.id);
      notify.success(result?.archived ? 'Мұрағатталды' : 'Жойылды');
      await load();
    } catch (err) {
      notify.error(err.message);
    }
  }
}

function insertAtCursor(field, text) {
  const start = field.selectionStart ?? field.value.length;
  const end = field.selectionEnd ?? field.value.length;

  field.value = field.value.slice(0, start) + text + field.value.slice(end);
  field.focus();
  field.selectionStart = start + text.length;
  field.selectionEnd = start + text.length;
}

function truncate(text, max) {
  const flat = text.replace(/\s+/g, ' ').trim();
  return flat.length > max ? `${flat.slice(0, max)}…` : flat;
}

function typeTone(type) {
  if (type === 'VOICE') return 'info';
  if (type === 'TEXT') return 'neutral';
  return 'success';
}
