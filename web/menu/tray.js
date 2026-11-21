(() => {
  const app = window.Telegram?.WebApp;
  const root = document.querySelector('#tray');
  const content = document.querySelector('#tray-content');
  const auth = { 'X-Telegram-Init-Data': app?.initData || '' };
  const labels = { BREAKFAST: 'Завтрак', LUNCH: 'Обед', DINNER: 'Ужин' };
  const statuses = { queued: 'В очереди', review: 'На проверке', closed: 'Завершено' };
  let menu, frameURL = '', detailVersion = 0;
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
  window.addEventListener('pagehide', revoke);
  async function api(path, options = {}) {
    const response = await fetch(path, { ...options, headers: { ...auth, ...options.headers }, cache: 'no-store' });
    if (!response.ok) {
      const known = { 409: options.body instanceof Blob ? 'Этот файл уже предложен для этого приёма пищи.' : 'Данные изменились. Обновите страницу и попробуйте снова.', 429: 'Превышен лимит загрузок. Попробуйте позже.', 415: 'Поддерживаются только JPEG и PNG.', 413: 'Файл слишком большой.' };
      throw new Error(known[response.status] || (await response.text()) || 'Ошибка: ' + response.status);
    }
    return response;
  }
  const message = (target, error) => { target.textContent = error.message || 'Произошла ошибка.'; };
  function summaryText(item) {
    return `${item.date} · ${labels[item.meal] || item.meal} · ${statuses[item.status] || item.status}`;
  }
  async function showDetail(id, target) {
    const version = ++detailVersion;
    revoke(); target.replaceChildren(node('p', 'Загрузка…'));
    try {
      const item = await (await api('/api/tray/' + encodeURIComponent(id))).json();
      if (version !== detailVersion) return;
      target.replaceChildren(node('h3', summaryText(item)));
      if (item.result?.length) target.append(node('p', 'Опубликовано: ' + item.result.join(', ')));
      else if (item.status === 'closed') target.append(node('p', 'Фото не опубликовано.'));
      if (item.reason) target.append(node('p', item.reason, 'note'));
      if (!menu.photo_admin || item.status !== 'review') return;
      const frame = await (await api('/api/tray/' + encodeURIComponent(id) + '/frame')).blob();
      if (version !== detailVersion) return;
      frameURL = URL.createObjectURL(frame);
      editor(item, target, frameURL);
    } catch (error) { if (version === detailVersion) message(target, error); }
  }
  function editor(item, target, url) {
    const dishes = item.dishes || [];
    const crops = (item.suggestions || []).map(s => ({ ...s, selected: !!s.selected, replace: false }));
    const error = node('p', '', 'tray-error'); error.setAttribute('role', 'status');
    const stage = node('div', '', 'crop-stage');
    const img = node('img'); img.src = url; img.alt = 'Фото подноса для разметки'; img.draggable = false;
    const boxes = node('div', '', 'crop-boxes'); stage.append(img, boxes);
    const list = node('div', '', 'crop-list');
    target.append(node('p', 'Выделите область пальцем на фото или добавьте её вручную. Координаты можно изменить ниже.'), stage, list);
    function draw() {
      boxes.replaceChildren(); list.replaceChildren();
      crops.forEach((crop, i) => {
        const box = node('div', String(i + 1), 'crop-box');
        box.style.cssText = `left:${crop.x / 10}%;top:${crop.y / 10}%;width:${crop.w / 10}%;height:${crop.h / 10}%`;
        boxes.append(box);
        const row = node('div', '', 'crop-row');
        const selectLabel = node('label', ' Использовать область ' + (i + 1));
        const selected = document.createElement('input'); selected.type = 'checkbox'; selected.checked = crop.selected;
        selected.addEventListener('change', () => { crop.selected = selected.checked; }); selectLabel.prepend(selected); row.append(selectLabel);
        const dishLabel = node('label', 'Блюдо '); const select = document.createElement('select');
        const empty = node('option', 'Выберите блюдо'); empty.value = ''; select.append(empty);
        dishes.forEach(dish => { const option = node('option', dish.name); option.value = String(dish.id); select.append(option); });
        select.value = String(crop.dish_id || '');
        select.addEventListener('change', () => { crop.dish_id = select.value; crop.replace = false; draw(); });
        dishLabel.append(select); row.append(dishLabel);
        const fields = node('div', '', 'crop-fields');
        [['x', 'Слева'], ['y', 'Сверху'], ['w', 'Ширина'], ['h', 'Высота']].forEach(([key, label]) => {
          const wrap = node('label', label); const input = document.createElement('input');
          input.type = 'number'; input.min = key === 'w' || key === 'h' ? '1' : '0'; input.max = '1000'; input.step = '1'; input.value = crop[key];
          input.addEventListener('input', () => { crop[key] = Number(input.value); box.style.cssText = `left:${crop.x / 10}%;top:${crop.y / 10}%;width:${crop.w / 10}%;height:${crop.h / 10}%`; });
          wrap.append(input); fields.append(wrap);
        });
        row.append(fields);
        const dish = dishes.find(d => String(d.id) === String(crop.dish_id));
        if (dish?.photo_id) {
          const replaceLabel = node('label', ' Явно заменить существующее фото');
          const replace = document.createElement('input'); replace.type = 'checkbox'; replace.checked = crop.replace;
          replace.addEventListener('change', () => { crop.replace = replace.checked; }); replaceLabel.prepend(replace); row.append(replaceLabel);
        }
        if (crop.reason) row.append(node('p', crop.reason, 'note'));
        row.append(button('Удалить область', () => { crops.splice(i, 1); draw(); })); list.append(row);
      });
    }
    let start;
    const point = event => { const r = stage.getBoundingClientRect(); return { x: Math.max(0, Math.min(1000, Math.round((event.clientX - r.left) / r.width * 1000))), y: Math.max(0, Math.min(1000, Math.round((event.clientY - r.top) / r.height * 1000))) }; };
    stage.addEventListener('pointerdown', event => { start = point(event); stage.setPointerCapture(event.pointerId); });
    stage.addEventListener('pointercancel', () => { start = null; });
    stage.addEventListener('pointerup', event => {
      if (!start) return; const end = point(event);
      const x = Math.min(start.x, end.x), y = Math.min(start.y, end.y), w = Math.abs(start.x - end.x), h = Math.abs(start.y - end.y); start = null;
      if (w && h) { crops.push({ x, y, w, h, dish_id: '', selected: true, replace: false }); draw(); }
    });
    const add = button('Добавить область вручную', () => { crops.push({ x: 100, y: 100, w: 400, h: 400, dish_id: '', selected: true, replace: false }); draw(); });
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
        revoke(); await showDetail(item.id, target);
      } catch (e) { message(error, e); submit.disabled = close.disabled = false; }
    }
    target.append(add, submit, close, error); draw();
  }
  function render() {
    ++detailVersion; revoke(); content.replaceChildren();
    root.hidden = !menu?.photos_enabled;
    if (root.hidden) return;
    content.append(node('p', menu.photo_ai_enabled ? 'Фото может быть отправлено ИИ для предложения областей; администратор проверит результат.' : 'Фото проверит администратор вручную; ИИ не используется.', 'note'));
    const form = document.createElement('form'); form.className = 'tray-form';
    const mealLabel = node('label', 'Приём пищи '); const meal = document.createElement('select');
    Object.keys(menu.meals).filter(key => menu.meals[key].some(d => d.kind === 'dish')).forEach(key => { const opt = node('option', labels[key] || key); opt.value = key; meal.append(opt); }); mealLabel.append(meal);
    const fileLabel = node('label', 'Фото JPEG или PNG '); const file = document.createElement('input'); file.type = 'file'; file.accept = 'image/jpeg,image/png'; file.required = true; fileLabel.append(file);
    const upload = node('button', 'Отправить фото'); upload.type = 'submit'; const uploadStatus = node('p', '', 'tray-error'); uploadStatus.setAttribute('role', 'status');
    form.append(mealLabel, fileLabel, upload, uploadStatus); content.append(form);
    form.hidden = !meal.options.length;
    form.dataset.trayUpload = '';
    const heading = node('h3', 'Мои заявки'); const own = node('div'); const detail = node('div', '', 'tray-detail'); content.append(heading, own);
    let queue;
    if (menu.photo_admin) { content.append(node('h3', 'Очередь модерации')); queue = node('div'); content.append(queue); }
    content.append(detail);
    async function list(target, moderation) {
      target.replaceChildren(node('p', 'Загрузка…', 'note'));
      try {
        const items = await (await api('/api/tray' + (moderation ? '?moderation=1' : ''))).json();
        target.replaceChildren();
        if (!items.length) target.append(node('p', 'Заявок пока нет.', 'note'));
        items.forEach(item => { const row = node('div', '', 'tray-summary'); row.append(button(summaryText(item), () => showDetail(item.id, detail))); if (item.result?.length) row.append(node('p', 'Опубликовано: ' + item.result.join(', '), 'note')); if (item.reason) row.append(node('p', item.reason, 'note')); target.append(row); });
      } catch (e) { message(target, e); }
    }
    form.addEventListener('submit', async event => {
      event.preventDefault(); if (!file.files.length) return;
      upload.disabled = true; uploadStatus.textContent = 'Отправка…';
      try {
        const item = await (await api('/api/tray?date=' + encodeURIComponent(menu.date) + '&meal=' + encodeURIComponent(meal.value), { method: 'POST', headers: { 'Content-Type': file.files[0].type || 'application/octet-stream' }, body: file.files[0] })).json();
        file.value = ''; uploadStatus.textContent = 'Фото отправлено.'; list(own, false); if (queue) list(queue, true); showDetail(item.id, detail);
      } catch (e) { message(uploadStatus, e); } finally { upload.disabled = false; }
    });
    list(own, false); if (queue) list(queue, true);
    const deep = new URLSearchParams(location.search).get('tray') || /^tray_(\d+)$/.exec(app?.initDataUnsafe?.start_param || '')?.[1];
    if (deep && /^\d+$/.test(deep)) showDetail(deep, detail);
  }
  window.addEventListener('menu:loaded', event => { menu = event.detail; render(); });
  window.addEventListener('menu:propose', event => {
    const form = content.querySelector('[data-tray-upload]');
    if (!form) return;
    form.querySelector('select').value = event.detail;
    form.scrollIntoView({ block: 'center' });
    form.querySelector('input').focus({ preventScroll: true });
  });
  window.addEventListener('menu:dish', event => {
    const { dish, content: target } = event.detail;
    if (!menu?.photos_enabled || !menu.photo_admin || dish.image_kind !== 'cafeteria' || !dish.photo_id) return;
    const error = node('p', '', 'tray-error');
    const remove = button('Удалить фото столовой', async () => {
      if (!confirm('Удалить одобренное фото этого блюда?')) return;
      remove.disabled = true;
      try { await api('/api/tray/dish/' + encodeURIComponent(dish.id) + '?photo_id=' + encodeURIComponent(dish.photo_id), { method: 'DELETE' }); window.dispatchEvent(new Event('menu:photo-removed')); }
      catch (e) { message(error, e); remove.disabled = false; }
    });
    target.append(remove, error);
  });
})();
