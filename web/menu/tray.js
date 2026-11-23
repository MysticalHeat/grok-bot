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
        if (result?.code === 'tray_ai_budget') throw new Error(result.message);
        throw new Error(result?.code === 'meal_photo_limit'
          ? 'Для этого приёма пищи уже отправили три фото. Выберите другой приём пищи или дату.'
          : 'Слишком много запросов. Попробуйте позже.');
      }
      const known = { 401: 'Откройте Mini App заново через Telegram-бота.', 403: 'Эта страница доступна только модератору.', 409: options.body instanceof Blob ? 'Этот файл уже предложен для этого приёма пищи.' : 'Данные изменились. Обновите страницу и попробуйте снова.', 415: 'Поддерживаются только JPEG и PNG.', 413: 'Файл слишком большой.' };
      throw new Error(known[response.status] || (await response.text()) || 'Ошибка: ' + response.status);
    }
    return response;
  }
  const message = (target, error) => { target.textContent = error.message || 'Произошла ошибка.'; };
  function summaryText(item) {
    const status = item.scan_running ? 'ИИ сканирует' : statuses[item.status] || item.status;
    return `${dateLabel(item.date)}, ${labels[item.meal] || item.meal} — ${status}`;
  }
  function scanHistory(path, open) {
    const section = node('details', '', 'tray-disclosure tray-history');
    const body = node('div');
    section.append(node('summary', 'История ИИ-сканов'), body);
    let version = 0;
    section.addEventListener('toggle', async () => {
      const request = ++version;
      if (!section.open) return;
      const visible = () => request === version && section.open && section.isConnected;
      body.replaceChildren(node('p', 'Загрузка истории…', 'note'));
      try {
        const entries = await (await api(path + '/history')).json();
        if (!visible()) return;
        if (!entries.length) {
          body.replaceChildren(node('p', 'Нет сохранённых попыток за последние 30 дней.', 'note'));
          return;
        }
        const timeline = node('ol');
        for (const entry of entries) {
          const row = node('li');
          const date = new Date(entry.started_at);
          const time = node('time', date.toLocaleString('ru-RU'));
          time.dateTime = entry.started_at;
          row.append(time, node('strong', entry.attempt > 1 ? 'Автоматический повтор' : 'ИИ-скан'));
          const meta = entry.finish_reason ? `${entry.model} (${entry.finish_reason})` : entry.model;
          row.append(node('p', meta, 'note'), node('p', entry.reason || 'Разметка получена.'));
          if (entry.response) {
            const response = node('details');
            response.append(node('summary', entry.response_truncated ? 'Ответ модели (сохранён не полностью)' : 'Ответ модели'));
            response.append(node('pre', entry.response));
            row.append(response);
          }
          timeline.append(row);
        }
        body.replaceChildren(timeline);
      } catch (error) {
        if (visible()) message(body, error);
      }
    });
    section.open = open;
    return section;
  }
  async function showDetail(id, target) {
    if (!target.isConnected || (!moderation && !root.open)) return;
    const version = ++detailVersion;
    const visible = () => version === detailVersion && target.isConnected && (moderation || root.open);
    const path = '/api/tray/' + encodeURIComponent(id);
    const load = async () => (await api(path)).json();
    revoke(); target.replaceChildren(node('p', 'Загрузка…'));
    try {
      await render(await load(), true);
    } catch (error) { if (visible()) message(target, error); }

    async function render(item, scroll) {
      if (!visible()) return;
      const historyOpen = target.querySelector('.tray-history')?.open ?? false;
      target.replaceChildren(node('h3', summaryText(item)));
      if (scroll) target.scrollIntoView({ block: 'start' });
      if (item.result?.length) target.append(node('p', 'Опубликовано: ' + item.result.join(', ')));
      else if (item.status === 'closed') target.append(node('p', 'Фото не опубликовано.'));
      if (item.reason) target.append(node('p', item.reason, 'note'));
      if (!moderation) return;
      target.append(scanHistory(path, historyOpen));
      if (item.status === 'queued') {
        const status = node('p', '', 'note');
        status.setAttribute('role', 'status');
        target.append(status);
        function wait(running) {
          status.textContent = running ? 'ИИ проверяет фото. Результат появится здесь.' : 'Фото ждёт ИИ-скана. Результат появится здесь.';
          setTimeout(async () => {
            if (!visible()) return;
            try {
              const latest = await load();
              if (!visible()) return;
              if (latest.status === 'queued') {
                target.firstElementChild.textContent = summaryText(latest);
                wait(latest.scan_running);
                return;
              }
              await render(latest, false);
              refreshQueue();
            } catch (error) {
              if (!visible()) return;
              message(status, error);
              status.after(button('Проверить статус', () => showDetail(id, target)));
            }
          }, 3000);
        }
        wait(item.scan_running);
        return;
      }
      if (item.status !== 'review') return;
      try {
        const frame = await (await api(path + '/frame')).blob();
        if (!visible()) return;
        frameURL = URL.createObjectURL(frame);
      } catch {
        if (!visible()) return;
        target.append(node('p', 'Не удалось открыть исходное фото. Попробуйте открыть заявку заново.', 'tray-error'));
      }
      editor(item, target, frameURL);
    }
  }
  function placeBox(box, { x, y, w, h }) {
    Object.assign(box.style, { left: `${x / 10}%`, top: `${y / 10}%`, width: `${w / 10}%`, height: `${h / 10}%` });
  }
  function orientPhoto(stage, img, rotation) {
    const aspect = img.naturalWidth / img.naturalHeight;
    const sideways = rotation % 2 === 1;
    stage.style.setProperty('--crop-aspect', sideways ? 1 / aspect : aspect);
    // The image keeps its original aspect; the stage fits its rotated bounds.
    stage.style.setProperty('--photo-width', `${sideways ? 100 * aspect : 100}%`);
    stage.style.setProperty('--photo-angle', `${rotation * 90}deg`);
  }
  function validCrop(c) {
    return ['x', 'y', 'w', 'h'].every(k => Number.isInteger(c[k])) &&
      c.x >= 0 && c.y >= 0 && c.w > 0 && c.h > 0 && c.x + c.w <= 1000 && c.y + c.h <= 1000;
  }
  const clamp = (value, min, max) => Math.min(max, Math.max(min, value));
  function adjustCrop({ x, y, w, h }, grip, dx, dy) {
    if (grip === 'move') return { x: clamp(x + dx, 0, 1000 - w), y: clamp(y + dy, 0, 1000 - h), w, h };
    const right = x + w, bottom = y + h;
    const left = grip.includes('w') ? clamp(x + dx, 0, right - 1) : x;
    const top = grip.includes('n') ? clamp(y + dy, 0, bottom - 1) : y;
    return {
      x: left, y: top,
      w: (grip.includes('e') ? clamp(right + dx, x + 1, 1000) : right) - left,
      h: (grip.includes('s') ? clamp(bottom + dy, y + 1, 1000) : bottom) - top,
    };
  }
  function photoEditor(target, source, crops, rotation, active, dishName, apply) {
    const draft = crops.map(c => ({ ...c }));
    const dialog = node('dialog', '', 'photo-editor');
    dialog.setAttribute('aria-labelledby', 'photo-editor-title');
    const title = node('h2', 'Редактор фото подноса');
    title.id = 'photo-editor-title';
    const controls = node('div', '', 'photo-editor-controls');
    const left = button('↶ Влево на 90°', () => turn(-1));
    const right = button('↷ Вправо на 90°', () => turn(1));
    const angles = node('div', '', 'photo-editor-angles');
    angles.append(left, right);
    const picker = node('label', 'Область');
    const select = node('select');
    picker.append(select);
    picker.hidden = !draft.length;
    controls.append(angles, picker);
    const help = node('p', draft.length
      ? 'Выберите рамку. Перетащите её целиком или потяните за угол.'
      : 'Здесь можно повернуть снимок. Добавить области можно в карточке заявки.', 'photo-editor-help');
    const well = node('div', '', 'photo-editor-well');
    const stage = node('div', '', 'crop-stage photo-editor-stage');
    const img = node('img');
    img.alt = source.alt;
    img.draggable = false;
    img.src = source.src;
    const boxes = node('div', '', 'crop-boxes');
    const handles = node('div', '', 'photo-editor-handles');
    handles.hidden = !draft.length;
    stage.append(img, boxes, handles);
    well.append(stage);

    const targets = draft.map((c, i) => {
      const name = `${i + 1}. ${dishName(c)}`;
      const option = node('option', name);
      option.value = String(i);
      select.append(option);
      const box = button(String(i + 1), () => choose(i));
      box.className = 'crop-box crop-target';
      box.setAttribute('aria-label', name);
      boxes.append(box);
      return box;
    });
    for (const [grip, label] of [['nw', 'Верхний левый угол'], ['ne', 'Верхний правый угол'], ['sw', 'Нижний левый угол'], ['se', 'Нижний правый угол']]) {
      const handle = node('button');
      handle.type = 'button';
      handle.dataset.grip = grip;
      handle.setAttribute('aria-label', label);
      handles.append(handle);
    }
    let gesture;
    function draw() {
      orientPhoto(stage, source, rotation);
      targets.forEach((box, i) => {
        placeBox(box, draft[i]);
        box.setAttribute('aria-pressed', String(i === active));
        if (i === active) box.dataset.grip = 'move';
        else delete box.dataset.grip;
      });
      if (targets[active]) placeBox(handles, draft[active]);
      select.value = String(active);
    }
    function choose(i) {
      finishGesture(true);
      active = i;
      draw();
    }
    select.addEventListener('change', () => choose(Number(select.value)));
    function turn(direction) {
      finishGesture(true);
      rotation = (rotation + direction + 4) % 4;
      for (const c of draft) {
        const { x, y, w, h } = c;
        Object.assign(c, direction === 1
          ? { x: 1000 - y - h, y: x, w: h, h: w }
          : { x: y, y: 1000 - x - w, w: h, h: w });
      }
      draw();
    }
    function finishGesture(cancel) {
      if (!gesture) return;
      const { id, start } = gesture;
      gesture = undefined;
      if (stage.hasPointerCapture(id)) stage.releasePointerCapture(id);
      if (cancel) {
        Object.assign(draft[active], start);
        draw();
      }
    }
    stage.addEventListener('pointerdown', event => {
      const grip = event.target.closest('[data-grip]')?.dataset.grip;
      if (!grip || gesture || !event.isPrimary || event.button !== 0) return;
      const { width, height } = stage.getBoundingClientRect();
      if (!width || !height) return;
      gesture = { grip, id: event.pointerId, start: { ...draft[active] }, width, height, x: event.clientX, y: event.clientY };
      stage.setPointerCapture(event.pointerId);
    });
    function move(event) {
      const { grip, start, width, height, x, y } = gesture;
      const dx = Math.round((event.clientX - x) * 1000 / width);
      const dy = Math.round((event.clientY - y) * 1000 / height);
      Object.assign(draft[active], adjustCrop(start, grip, dx, dy));
      placeBox(targets[active], draft[active]);
      placeBox(handles, draft[active]);
    }
    stage.addEventListener('pointermove', event => { if (gesture?.id === event.pointerId) move(event); });
    stage.addEventListener('pointerup', event => {
      if (gesture?.id !== event.pointerId) return;
      move(event);
      finishGesture(false);
    });
    for (const eventName of ['pointercancel', 'lostpointercapture']) {
      stage.addEventListener(eventName, event => { if (gesture?.id === event.pointerId) finishGesture(true); });
    }
    stage.addEventListener('keydown', event => {
      const grip = event.target.dataset.grip;
      const delta = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, -1], ArrowDown: [0, 1] }[event.key];
      if (!grip || !delta || gesture) return;
      event.preventDefault();
      const step = event.shiftKey ? 1 : 10;
      Object.assign(draft[active], adjustCrop(draft[active], grip, delta[0] * step, delta[1] * step));
      draw();
    });
    const actions = node('div', '', 'photo-editor-actions');
    const cancel = button('Отмена', () => dialog.close());
    const done = button('Готово', () => {
      finishGesture(true);
      dialog.close();
      apply(draft, rotation);
    });
    actions.append(cancel, done);
    dialog.append(title, controls, help, well, actions);
    dialog.addEventListener('close', () => dialog.remove(), { once: true });
    target.append(dialog);
    draw();
    dialog.showModal();
  }
  function editor(item, target, url) {
    const dishes = item.dishes || [];
    const findDish = id => dishes.find(d => String(d.id) === String(id));
    const crops = (item.suggestions || []).map(s => ({
      ...s, selected: !!s.selected && !findDish(s.dish_id)?.photo_id, replace: false,
    }));
    const original = JSON.stringify(crops);
    let rotation = 0;
    const error = node('p', '', 'tray-error'); error.setAttribute('role', 'status');
    const stage = node('div', '', 'crop-stage');
    const img = node('img'); img.alt = 'Фото подноса для разметки'; img.draggable = false;
    const boxes = node('div', '', 'crop-boxes');
    const draft = node('div', '', 'crop-box crop-draft'); draft.hidden = true; draft.setAttribute('aria-hidden', 'true');
    const label = node('div', '', 'crop-label'); label.hidden = true; label.setAttribute('aria-hidden', 'true');
    stage.append(img, boxes, draft, label);
    const list = node('div', '', 'crop-list');
    const inspectHint = 'Наведите курсор на область, чтобы увидеть блюдо. Нажмите на область, чтобы перейти к её форме. Свайп по фото прокручивает страницу.';
    const hint = node('p', inspectHint, 'crop-hint');
    hint.setAttribute('role', 'status');
    const tools = node('div', '', 'crop-tools');
    const editorError = node('p', '', 'tray-error'); editorError.setAttribute('role', 'status');
    if (url) target.append(tools, editorError, hint, stage, list);
    const openEditor = button('Открыть редактор', () => openPhotoEditor());
    openEditor.className = 'crop-open-editor';
    openEditor.disabled = true;
    function openPhotoEditor(i = -1) {
      if (openEditor.disabled) return;
      if (crops.some(c => !validCrop(c))) {
        editorError.textContent = 'Проверьте числовые координаты: все рамки должны быть внутри фото.';
        editorError.scrollIntoView({ block: 'nearest' });
        return;
      }
      editorError.textContent = '';
      setDrawing(false);
      const initial = i >= 0 ? i : Math.max(0, crops.findIndex(c => c.selected));
      photoEditor(target, img, crops, rotation, initial, c => findDish(c.dish_id)?.name || 'Блюдо не выбрано', (draft, turns) => {
        crops.splice(0, crops.length, ...draft);
        rotation = turns;
        orientPhoto(stage, img, rotation);
        draw();
        (list.children[i]?.querySelector('.crop-edit') || openEditor).focus({ preventScroll: true });
      });
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
        const row = node('div', '', 'crop-row');
        const selected = document.createElement('input'); selected.type = 'checkbox'; selected.checked = crop.selected;
        const box = button(String(i + 1), () => {
          if (drawing) return;
          selected.focus({ preventScroll: true });
          row.scrollIntoView({ block: 'start' });
        });
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
        const selectLabel = node('label', ' Использовать область ' + (i + 1));
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
          input.addEventListener('input', () => {
            crop[key] = input.valueAsNumber;
            placeBox(box, crop);
            editorError.textContent = '';
          });
          wrap.append(input); fields.append(wrap);
        });
        row.append(fields);
        const replaceLabel = node('label', ' Явно заменить существующее фото');
        const replace = document.createElement('input'); replace.type = 'checkbox'; replace.checked = crop.replace;
        replace.addEventListener('change', () => { crop.replace = replace.checked; }); replaceLabel.prepend(replace); row.append(replaceLabel);
        function updateDish(dish) {
          box.setAttribute('aria-label', `Область ${i + 1}: ${dish?.name || 'Блюдо не выбрано'}`);
          replaceLabel.hidden = !dish?.photo_id;
          if (activeBox === box) label.textContent = box.getAttribute('aria-label');
        }
        select.addEventListener('change', () => {
          crop.dish_id = select.value;
          crop.replace = replace.checked = false;
          const dish = findDish(crop.dish_id);
          if (dish?.photo_id) crop.selected = selected.checked = false;
          updateDish(dish);
        });
        updateDish(findDish(crop.dish_id));
        if (crop.reason) row.append(node('p', crop.reason, 'note'));
        const edit = button('Изменить рамку', () => openPhotoEditor(i));
        edit.className = 'crop-edit';
        const rowActions = node('div', '', 'crop-tools');
        rowActions.append(edit, button('Удалить область', () => { crops.splice(i, 1); draw(); }));
        row.append(rowActions); list.append(row);
      });
    }
    let gesture;
    const add = button('Выделить на фото', () => setDrawing(!drawing));
    add.disabled = true;
    add.setAttribute('aria-pressed', 'false');
    img.addEventListener('load', () => {
      orientPhoto(stage, img, rotation);
      openEditor.disabled = add.disabled = false;
    });
    img.addEventListener('error', () => { hint.textContent = 'Не удалось открыть фото. Попробуйте открыть заявку заново.'; });
    if (url) img.src = url;

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
    tools.append(openEditor, add, manual);
    const submit = button('Опубликовать выбранные области', () => decide(false));
    const close = button('Закрыть без публикации', () => decide(true));
    const rescan = button('Повторить ИИ-скан', async () => {
      if ((rotation || JSON.stringify(crops) !== original) && !confirm('Повторный ИИ-скан заменит разметку. Несохранённые правки будут потеряны. Продолжить?')) return;
      setBusy(true);
      try {
        await api('/api/tray/' + encodeURIComponent(item.id) + '/rescan', { method: 'POST' });
        if (target.contains(error)) await showDetail(item.id, target);
        refreshQueue();
      } catch (e) { setBusy(false); message(error, e); }
    });
    function setBusy(busy) {
      submit.disabled = !url || busy;
      close.disabled = rescan.disabled = busy;
      tools.inert = stage.inert = list.inert = busy;
      if (busy) error.textContent = '';
    }
    async function decide(empty) {
      const chosen = empty ? [] : crops.filter(c => c.selected);
      if (!empty && !chosen.length) { error.textContent = 'Выберите хотя бы одну область или закройте без публикации.'; return; }
      const ids = new Set();
      for (const c of chosen) {
        const dish = findDish(c.dish_id);
        if (!dish || ids.has(String(dish.id)) || !validCrop(c)) { error.textContent = 'Проверьте блюдо и координаты областей: размеры должны быть положительными, области — внутри фото, блюда не должны повторяться.'; return; }
        if (dish.photo_id && !c.replace) { error.textContent = 'Для замены существующего фото отметьте явное согласие на замену.'; return; }
        ids.add(String(dish.id));
      }
      if (empty && !confirm('Закрыть заявку без публикации?')) return;
      setBusy(true);
      try {
        const publish = chosen.map(({ dish_id, x, y, w, h, replace }) => {
          const dish = findDish(dish_id);
          return { dish_id: dish.id, x, y, w, h, replace: replace ? dish.photo_id : '' };
        });
        await api('/api/tray/' + encodeURIComponent(item.id) + '/decision', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ rotation, crops: publish }),
        });
        if (target.contains(error)) await showDetail(item.id, target);
        refreshQueue();
        if (document.querySelector('#moderation-published').open) refreshPublished();
      } catch (e) { setBusy(false); message(error, e); }
    }
    const actions = node('div', '', 'crop-actions');
    if (item.can_rescan) actions.append(rescan);
    if (url) actions.append(submit);
    actions.append(close);
    target.append(actions, error);
    if (url) draw();
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
