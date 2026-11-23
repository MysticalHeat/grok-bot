(() => {
  const app = window.Telegram?.WebApp;
  const root = document.querySelector('#tray');
  const opener = document.querySelector('#tray-open');
  const content = document.querySelector('#tray-content');
  const moderation = document.querySelector('#moderation');
  const auth = { 'X-Telegram-Init-Data': app?.initData || '' };
  const labels = { BREAKFAST: 'Завтрак', LUNCH: 'Обед', DINNER: 'Ужин' };
  const statuses = { queued: 'В очереди', review: 'На проверке', closed: 'Завершено' };
  let menu, frameURL = '', previewURL = '', detailVersion = 0;
  const node = (tag, text, className) => {
    const el = document.createElement(tag);
    if (text) el.textContent = text;
    if (className) el.className = className;
    return el;
  };
  const button = (text, action) => {
    const el = node('button', text); el.type = 'button'; el.addEventListener('click', action); return el;
  };
  const revoke = () => { if (frameURL) URL.revokeObjectURL(frameURL); frameURL = ''; };
  const revokePreview = () => { if (previewURL) URL.revokeObjectURL(previewURL); previewURL = ''; };
  window.addEventListener('pagehide', event => { if (!event.persisted) { revoke(); revokePreview(); } });
  const dateLabel = date => new Intl.DateTimeFormat('ru-RU', { dateStyle: 'long', timeZone: 'UTC' }).format(new Date(date + 'T12:00:00Z'));
  async function api(path, options = {}) {
    const response = await fetch(path, { ...options, headers: { ...auth, ...options.headers }, cache: 'no-store' });
    if (!response.ok) {
      if (response.status === 429) {
        const result = await response.json().catch(() => null);
        throw new Error(result?.code === 'meal_photo_limit'
          ? 'Для этого приёма пищи уже отправили три фото. Выберите другой приём пищи или дату.'
          : 'Вы достигли личного лимита загрузок на сегодня.');
      }
      const known = { 401: 'Откройте Mini App заново через Telegram-бота.', 403: 'Эта страница доступна только модератору.', 409: options.body instanceof Blob ? 'Этот файл уже предложен для этого приёма пищи.' : 'Данные изменились. Обновите страницу и попробуйте снова.', 415: 'Поддерживаются только JPEG и PNG.', 413: 'Файл слишком большой.' };
      throw new Error(known[response.status] || (await response.text()) || 'Ошибка: ' + response.status);
    }
    return response;
  }
  const message = (target, error) => { target.textContent = error.message || 'Произошла ошибка.'; };
  function summaryText(item) {
    return `${dateLabel(item.date)}, ${labels[item.meal] || item.meal} — ${statuses[item.status] || item.status}`;
  }
  async function showDetail(id, target) {
    const visible = () => target.isConnected && (moderation || root.open);
    if (!visible()) return;
    const version = ++detailVersion;
    revoke(); target.replaceChildren(node('p', 'Загрузка…'));
    try {
      const item = await (await api('/api/tray/' + encodeURIComponent(id))).json();
      if (!visible() || version !== detailVersion) return;
      target.replaceChildren(node('h3', summaryText(item)));
      target.scrollIntoView({ block: 'start' });
      if (item.result?.length) target.append(node('p', 'Опубликовано: ' + item.result.join(', ')));
      else if (item.status === 'closed') target.append(node('p', 'Фото не опубликовано.'));
      if (item.reason) target.append(node('p', item.reason, 'note'));
      if (!moderation || item.status !== 'review') return;
      const frame = await (await api('/api/tray/' + encodeURIComponent(id) + '/frame')).blob();
      if (!visible() || version !== detailVersion) return;
      frameURL = URL.createObjectURL(frame);
      editor(item, target, frameURL);
    } catch (error) { if (version === detailVersion) message(target, error); }
  }
  function editor(item, target, url) {
    const dishes = item.dishes || [];
    const crops = (item.suggestions || []).map(s => ({ ...s, selected: !!s.selected, replace: false }));
    const error = node('p', '', 'tray-error'); error.setAttribute('role', 'status');
    const stage = node('div', '', 'crop-stage');
    const img = node('img'); img.alt = 'Фото подноса для разметки'; img.draggable = false;
    const boxes = node('div', '', 'crop-boxes');
    const draft = node('div', '', 'crop-box crop-draft'); draft.hidden = true; draft.setAttribute('aria-hidden', 'true');
    const label = node('div', '', 'crop-label'); label.hidden = true; label.setAttribute('aria-hidden', 'true');
    stage.append(img, boxes, draft, label);
    const list = node('div', '', 'crop-list');
    const inspectHint = 'Наведите курсор на область или коснитесь её, чтобы увидеть блюдо. Свайп по фото прокручивает страницу.';
    const hint = node('p', inspectHint, 'crop-hint');
    hint.setAttribute('role', 'status');
    const tools = node('div', '', 'crop-tools');
    target.append(tools, hint, stage, list);
    function placeBox(box, { x, y, w, h }) {
      Object.assign(box.style, { left: `${x / 10}%`, top: `${y / 10}%`, width: `${w / 10}%`, height: `${h / 10}%` });
    }
    let drawing = false;
    let activeBox;
    function showLabel(box) {
      if (drawing) return;
      if (activeBox !== box) activeBox?.classList.remove('is-inspected');
      activeBox = box;
      box.classList.add('is-inspected');
      label.textContent = box.getAttribute('aria-label');
      label.hidden = false;
    }
    function hideLabel() {
      activeBox?.classList.remove('is-inspected');
      activeBox = undefined;
      label.hidden = true;
    }
    stage.addEventListener('click', event => { if (!boxes.contains(event.target)) hideLabel(); });
    stage.addEventListener('keydown', event => { if (event.key === 'Escape') hideLabel(); });
    function draw() {
      hideLabel();
      boxes.replaceChildren(); list.replaceChildren();
      crops.forEach((crop, i) => {
        const box = button(String(i + 1), () => showLabel(box));
        box.className = 'crop-box crop-target';
        placeBox(box, crop);
        let hovered = false;
        box.addEventListener('pointerenter', event => {
          if (event.pointerType === 'touch') return;
          hovered = true;
          showLabel(box);
        });
        box.addEventListener('pointerleave', event => {
          if (event.pointerType === 'touch') return;
          hovered = false;
          if (activeBox === box && !box.matches(':focus-visible')) hideLabel();
        });
        box.addEventListener('focus', () => { if (box.matches(':focus-visible')) showLabel(box); });
        box.addEventListener('blur', () => { if (activeBox === box && !hovered) hideLabel(); });
        boxes.append(box);
        const row = node('div', '', 'crop-row');
        const selectLabel = node('label', ' Использовать область ' + (i + 1));
        const selected = document.createElement('input'); selected.type = 'checkbox'; selected.checked = crop.selected;
        selected.addEventListener('change', () => { crop.selected = selected.checked; }); selectLabel.prepend(selected); row.append(selectLabel);
        const dishLabel = node('label', 'Блюдо '); const select = document.createElement('select');
        const empty = node('option', 'Выберите блюдо'); empty.value = ''; select.append(empty);
        dishes.forEach(dish => { const option = node('option', dish.name); option.value = String(dish.id); select.append(option); });
        select.value = String(crop.dish_id || '');
        dishLabel.append(select); row.append(dishLabel);
        const fields = node('div', '', 'crop-fields');
        [['x', 'Слева'], ['y', 'Сверху'], ['w', 'Ширина'], ['h', 'Высота']].forEach(([key, label]) => {
          const wrap = node('label', label); const input = document.createElement('input');
          input.type = 'number'; input.min = key === 'w' || key === 'h' ? '1' : '0'; input.max = '1000'; input.step = '1'; input.value = crop[key];
          input.addEventListener('input', () => { crop[key] = Number(input.value); placeBox(box, crop); });
          wrap.append(input); fields.append(wrap);
        });
        row.append(fields);
        const replaceLabel = node('label', ' Явно заменить существующее фото');
        const replace = document.createElement('input'); replace.type = 'checkbox'; replace.checked = crop.replace;
        replace.addEventListener('change', () => { crop.replace = replace.checked; }); replaceLabel.prepend(replace); row.append(replaceLabel);
        function updateDish() {
          const dish = dishes.find(d => String(d.id) === String(crop.dish_id));
          box.setAttribute('aria-label', `Область ${i + 1}: ${dish?.name || 'Блюдо не выбрано'}`);
          replaceLabel.hidden = !dish?.photo_id;
          if (activeBox === box) label.textContent = box.getAttribute('aria-label');
        }
        select.addEventListener('change', () => {
          crop.dish_id = select.value;
          crop.replace = replace.checked = false;
          updateDish();
        });
        updateDish();
        if (crop.reason) row.append(node('p', crop.reason, 'note'));
        row.append(button('Удалить область', () => { crops.splice(i, 1); draw(); })); list.append(row);
      });
    }
    let gesture;
    const add = button('Выделить на фото', () => setDrawing(!drawing));
    add.disabled = true;
    add.setAttribute('aria-pressed', 'false');
    img.addEventListener('load', () => {
      stage.style.setProperty('--crop-aspect', img.naturalWidth / img.naturalHeight);
      add.disabled = false;
    });
    img.addEventListener('error', () => { hint.textContent = 'Не удалось открыть фото. Попробуйте открыть заявку заново.'; });
    img.src = url;

    function clearGesture() {
      const id = gesture?.id;
      gesture = undefined;
      draft.hidden = true;
      if (id !== undefined && stage.hasPointerCapture(id)) stage.releasePointerCapture(id);
    }
    function setDrawing(active) {
      clearGesture();
      drawing = active;
      hideLabel();
      boxes.inert = active;
      stage.classList.toggle('is-drawing', active);
      add.textContent = active ? 'Отменить выделение' : 'Выделить на фото';
      add.setAttribute('aria-pressed', String(active));
      hint.textContent = active
        ? 'Проведите по диагонали от одного угла области к другому.'
        : inspectHint;
    }
    function point(event, bounds) {
      const unit = (offset, size) => Math.max(0, Math.min(1000, Math.round(offset / size * 1000)));
      return { x: unit(event.clientX - bounds.left, bounds.width), y: unit(event.clientY - bounds.top, bounds.height) };
    }
    function dragArea(event) {
      const { bounds, start } = gesture;
      const end = point(event, bounds);
      const w = Math.abs(start.x - end.x);
      const h = Math.abs(start.y - end.y);
      // Ignore taps, jitter and almost horizontal/vertical swipes.
      const minPixels = 12;
      if (w * bounds.width / 1000 < minPixels || h * bounds.height / 1000 < minPixels) return null;
      return { x: Math.min(start.x, end.x), y: Math.min(start.y, end.y), w, h };
    }
    stage.addEventListener('pointerdown', event => {
      if (!drawing || gesture || !event.isPrimary || event.button !== 0) return;
      const bounds = stage.getBoundingClientRect();
      if (!bounds.width || !bounds.height) return;
      gesture = { id: event.pointerId, bounds, start: point(event, bounds) };
      stage.setPointerCapture(event.pointerId);
    });
    stage.addEventListener('pointermove', event => {
      if (gesture?.id !== event.pointerId) return;
      const area = dragArea(event);
      draft.hidden = !area;
      if (area) placeBox(draft, area);
    });
    stage.addEventListener('pointerup', event => {
      if (gesture?.id !== event.pointerId) return;
      const area = dragArea(event);
      clearGesture();
      if (!area) return;
      setDrawing(false);
      crops.push({ ...area, dish_id: '', selected: true, replace: false });
      draw();
    });
    for (const eventName of ['pointercancel', 'lostpointercapture']) {
      stage.addEventListener(eventName, event => { if (gesture?.id === event.pointerId) setDrawing(false); });
    }
    const manual = button('Ввести координаты', () => {
      setDrawing(false);
      crops.push({ x: 100, y: 100, w: 400, h: 400, dish_id: '', selected: true, replace: false });
      draw();
      list.lastElementChild?.querySelector('select')?.focus();
    });
    tools.append(add, manual);
    const submit = button('Опубликовать выбранные области', () => decide(false));
    const close = button('Закрыть без публикации', () => decide(true));
    async function decide(empty) {
      const chosen = empty ? [] : crops.filter(c => c.selected);
      if (!empty && !chosen.length) { error.textContent = 'Выберите хотя бы одну область или закройте без публикации.'; return; }
      const ids = new Set();
      for (const c of chosen) {
        const dish = dishes.find(d => String(d.id) === String(c.dish_id));
        if (!dish || ids.has(String(dish.id)) || !['x', 'y', 'w', 'h'].every(k => Number.isInteger(c[k])) || c.x < 0 || c.y < 0 || c.w <= 0 || c.h <= 0 || c.x + c.w > 1000 || c.y + c.h > 1000) { error.textContent = 'Проверьте блюдо и координаты областей: размеры должны быть положительными, области — внутри фото, блюда не должны повторяться.'; return; }
        if (dish.photo_id && !c.replace) { error.textContent = 'Для замены существующего фото отметьте явное согласие на замену.'; return; }
        ids.add(String(dish.id));
      }
      if (empty && !confirm('Закрыть заявку без публикации?')) return;
      submit.disabled = close.disabled = true; error.textContent = '';
      try {
        await api('/api/tray/' + encodeURIComponent(item.id) + '/decision', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ crops: chosen.map(c => { const dish = dishes.find(d => String(d.id) === String(c.dish_id)); return { dish_id: dish.id, x: c.x, y: c.y, w: c.w, h: c.h, replace: c.replace ? dish.photo_id : '' }; }) }) });
        if (target.contains(error)) await showDetail(item.id, target);
        refreshQueue();
        if (document.querySelector('#moderation-published').open) refreshPublished();
      } catch (e) { message(error, e); submit.disabled = close.disabled = false; }
    }
    const actions = node('div', '', 'crop-actions');
    actions.append(submit, close);
    target.append(actions, error); draw();
  }
  function openTray() {
    if (!menu?.photos_enabled || opener.disabled || root.open) return;
    const { date, meals, photo_ai_enabled } = menu;
    ++detailVersion; revoke(); revokePreview(); content.replaceChildren();
    content.append(node('p', 'Один снимок подноса поможет дополнить карточки нескольких блюд. На каждый приём пищи в выбранную дату можно отправить до трёх снимков суммарно от всех пользователей.', 'tray-intro'));
    const form = document.createElement('form'); form.className = 'tray-form';
    const context = node('div', '', 'tray-context');
    const day = node('div', '', 'tray-date');
    const time = node('time', dateLabel(date)); time.dateTime = date;
    day.append(node('span', 'Дата меню'), time);
    const mealLabel = node('label', 'Приём пищи'); const meal = document.createElement('select');
    for (const key of new Set([...Object.keys(labels), ...Object.keys(meals)])) {
      if (!meals[key]?.some(d => d.kind === 'dish')) continue;
      const option = node('option', labels[key] || key);
      option.value = key;
      meal.append(option);
    }
    mealLabel.append(meal);
    context.append(day, mealLabel);
    const file = document.createElement('input'); file.type = 'file'; file.accept = 'image/jpeg,image/png'; file.required = true;
    const preview = node('img', '', 'tray-preview'); preview.alt = 'Предпросмотр снимка подноса'; preview.hidden = true;
    const picker = node('label', '', 'tray-picker');
    const pickTitle = node('span', 'Выбрать снимок', 'tray-pick-title');
    const hint = node('span', 'JPEG или PNG, до 8 МБ', 'tray-file-hint');
    picker.append(file, preview, pickTitle, hint);
    const privacy = node('p', photo_ai_enabled ? 'Снимок может быть отправлен ИИ для выделения блюд. Фото появятся в меню только после проверки.' : 'Фото появятся в меню только после проверки модератором.', 'tray-file-hint');
    const upload = node('button', 'Отправить фото'); upload.type = 'submit'; upload.disabled = true;
    const uploadStatus = node('p', '', 'tray-upload-status'); uploadStatus.setAttribute('role', 'status');
    function updatePreview() {
      revokePreview();
      preview.hidden = true;
      preview.removeAttribute('src');
      const chosen = file.files[0];
      pickTitle.textContent = chosen ? 'Заменить снимок' : 'Выбрать снимок';
      hint.textContent = chosen ? chosen.name : 'JPEG или PNG, до 8 МБ';
      upload.disabled = !chosen;
      if (chosen) { previewURL = URL.createObjectURL(chosen); preview.src = previewURL; preview.hidden = false; }
    }
    preview.addEventListener('error', () => { preview.hidden = true; });
    file.addEventListener('change', () => { uploadStatus.textContent = ''; updatePreview(); });
    form.append(context, picker, privacy, upload, uploadStatus); content.append(form);
    form.hidden = !meal.options.length;
    if (form.hidden) content.append(node('p', 'На эту дату нет приёмов пищи для загрузки. Выберите другую дату в меню.', 'note'));
    const detail = node('div', '', 'tray-detail');
    content.append(detail);
    const history = node('details', '', 'tray-disclosure');
    const own = node('div');
    async function refreshHistory() {
      own.replaceChildren(node('p', 'Загрузка…', 'note'));
      try {
        renderSubmissions(own, await (await api('/api/tray')).json(), detail);
      } catch (e) { message(own, e); }
    }
    history.append(node('summary', 'Мои заявки'), own);
    history.addEventListener('toggle', () => { if (history.open) refreshHistory(); });
    content.insertBefore(history, detail);
    form.addEventListener('submit', async event => {
      event.preventDefault(); if (upload.disabled || !file.files.length) return;
      const chosen = file.files[0];
      uploadStatus.classList.remove('tray-error');
      if (chosen.size > 8 * 1024 * 1024) {
        uploadStatus.classList.add('tray-error'); uploadStatus.textContent = 'Выберите снимок не больше 8 МБ.'; return;
      }
      upload.disabled = file.disabled = meal.disabled = true;
      uploadStatus.textContent = 'Отправка…';
      try {
        const item = await (await api('/api/tray?date=' + encodeURIComponent(date) + '&meal=' + encodeURIComponent(meal.value), { method: 'POST', headers: { 'Content-Type': chosen.type || 'application/octet-stream' }, body: chosen })).json();
        if (!root.open || !form.isConnected) return;
        file.value = ''; updatePreview(); uploadStatus.textContent = 'Фото отправлено на проверку.';
        if (history.open) refreshHistory();
        showDetail(item.id, detail);
      } catch (e) { uploadStatus.classList.add('tray-error'); message(uploadStatus, e); }
      finally { file.disabled = meal.disabled = false; upload.disabled = !file.files.length; }
    });
    root.showModal();
    root.scrollTop = 0;
  }
  function renderSubmissions(target, items, detail) {
    target.replaceChildren();
    if (!items.length) target.append(node('p', 'Заявок пока нет.', 'note'));
    for (const item of items) {
      const row = node('div', '', 'tray-summary');
      row.append(button(summaryText(item), () => showDetail(item.id, detail)));
      if (item.result?.length) row.append(node('p', 'Опубликовано: ' + item.result.join(', '), 'note'));
      if (item.reason) row.append(node('p', item.reason, 'note'));
      target.append(row);
    }
  }
  async function refreshQueue() {
    const status = document.querySelector('#moderation-status');
    const panel = document.querySelector('#moderation-content');
    try {
      const items = await (await api('/api/tray?moderation=1')).json();
      renderSubmissions(document.querySelector('#moderation-queue'), items, document.querySelector('#moderation-detail'));
      status.textContent = '';
      panel.hidden = false;
      return true;
    } catch (e) {
      panel.hidden = true;
      message(status, e);
      return false;
    }
  }
  async function refreshPublished() {
    const target = document.querySelector('#moderation-photos');
    target.replaceChildren(node('p', 'Загрузка…', 'note'));
    try {
      const dishes = await (await api('/api/tray/published')).json();
      target.replaceChildren();
      if (!dishes.length) target.append(node('p', 'Опубликованных фото пока нет.', 'note'));
      for (const dish of dishes) {
        const row = node('div', '', 'moderation-photo');
        const img = node('img', '', 'thumb');
        img.src = '/api/tray-photo/' + encodeURIComponent(dish.photo_id);
        img.alt = ''; img.loading = 'lazy';
        const error = node('p', '', 'tray-error'); error.setAttribute('role', 'status');
        const remove = button('Снять фото', async () => {
          if (!confirm('Снять фото блюда «' + dish.name + '»?')) return;
          remove.disabled = true;
          try {
            await api('/api/tray/dish/' + encodeURIComponent(dish.id) + '?photo_id=' + encodeURIComponent(dish.photo_id), { method: 'DELETE' });
            refreshPublished();
          } catch (e) { message(error, e); remove.disabled = false; }
        });
        remove.setAttribute('aria-label', 'Снять фото блюда «' + dish.name + '»');
        row.append(img, node('strong', dish.name), remove, error);
        target.append(row);
      }
    } catch (e) { message(target, e); }
  }
  const params = new URLSearchParams(location.search);
  const start = app?.initDataUnsafe?.start_param || '';
  const trayID = params.get('tray') || /^tray_(\d+)$/.exec(start)?.[1];
  const deep = /^\d+$/.test(trayID || '') ? trayID : '';
  if (moderation) {
    app?.ready();
    app?.expand();
    document.querySelector('#moderation-back').href = '/?menu=1' + location.hash;
    const status = document.querySelector('#moderation-status');
    if (!app?.initData) { status.textContent = 'Откройте страницу через Telegram-бота.'; return; }
    const published = document.querySelector('#moderation-published');
    published.addEventListener('toggle', () => { if (published.open) refreshPublished(); });
    const refresh = document.querySelector('#moderation-refresh');
    refresh.addEventListener('click', async () => {
      refresh.disabled = true;
      await refreshQueue();
      refresh.disabled = false;
    });
    refreshQueue().then(allowed => {
      if (allowed && deep) showDetail(deep, document.querySelector('#moderation-detail'));
    });
    return;
  }
  opener.addEventListener('click', () => openTray());
  document.querySelector('#tray-close').addEventListener('click', () => root.close());
  root.addEventListener('close', () => {
    if (root.open) return;
    ++detailVersion; revoke(); revokePreview(); content.replaceChildren();
  });
  const moderationLink = document.querySelector('#moderation-link');
  moderationLink.href = '/moderation' + location.hash;
  window.addEventListener('menu:loading', () => { opener.disabled = true; });
  window.addEventListener('menu:loaded', event => {
    menu = event.detail;
    opener.hidden = !menu.photos_enabled;
    opener.disabled = false;
    moderationLink.hidden = !menu.photo_admin;
    if (menu.photo_admin && !params.has('menu') && (deep || start === 'moderation')) {
      location.replace('/moderation' + (deep ? '?tray=' + encodeURIComponent(deep) : '') + location.hash);
    }
  });
})();
