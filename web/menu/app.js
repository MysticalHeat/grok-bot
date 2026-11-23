(() => {
  const app = window.Telegram?.WebApp;
  app?.ready();
  app?.expand();
  const initData = app?.initData || '';
  const status = document.querySelector('#status');
  const mealsElement = document.querySelector('#meals');
  const dateElement = document.querySelector('#date');
  const dialog = document.querySelector('#dish-dialog');
  const dialogContent = document.querySelector('#dish-dialog-content');
  let date = '', requestedDate = '', request, photoAdmin = false, menuData, openPhoto;
  const thumbnails = new WeakMap();

  const element = (tag, text, className) => {
    const node = document.createElement(tag);
    if (text) node.textContent = text;
    if (className) node.className = className;
    return node;
  };
  const safeURL = value => {
    if (!value) return '';
    try { const url = new URL(value, location.href); return url.protocol === 'https:' || url.origin === location.origin ? url.href : ''; } catch { return ''; }
  };
  const link = (label, value) => {
    const href = safeURL(value);
    if (!href) return null;
    const a = element('a', label);
    a.href = href; a.target = '_blank'; a.rel = 'noopener noreferrer';
    return a;
  };
  const addLink = (parent, label, value) => {
    const a = link(label, value);
    if (a) { const p = element('p'); p.append(a); parent.append(p); }
  };
  const sameImage = (a, b) => a.id === b.id && a.photo_id === b.photo_id && a.image_url === b.image_url;
  function orientImage(frame, rotation = 0) {
    frame.style.setProperty('--dish-image-angle', `${rotation}deg`);
    frame.classList.toggle('is-quarter-turn', rotation % 180 !== 0);
  }
  function updateRotation(dish, rotation) {
    dish.image_rotation = rotation;
    for (const dishes of Object.values(menuData?.meals || {})) {
      for (const item of dishes) {
        if (!sameImage(item, dish)) continue;
        item.image_rotation = rotation;
        const thumb = thumbnails.get(item);
        if (thumb) orientImage(thumb, rotation);
      }
    }
    if (openPhoto && sameImage(openPhoto.dish, dish)) {
      openPhoto.dish.image_rotation = rotation;
      orientImage(openPhoto.frame, rotation);
    }
  }
  function dishImageControls(dish) {
    const tools = element('div', '', 'dish-image-tools');
    tools.setAttribute('role', 'group');
    tools.setAttribute('aria-label', 'Редактирование фото блюда');
    const feedback = element('p', '', 'note');
    feedback.setAttribute('role', 'status');
    const left = element('button', '↶ Влево');
    const right = element('button', '↷ Вправо');
    left.setAttribute('aria-label', 'Повернуть фото на 90° влево');
    right.setAttribute('aria-label', 'Повернуть фото на 90° вправо');
    const remove = element('button', 'Удалить фото', 'remove-dish-image');
    const buttons = [left, right, remove];
    for (const button of buttons) button.type = 'button';
    async function edit(rotation) {
      const removing = rotation === undefined;
      if (removing) {
        const name = dish.translated_name || dish.name;
        if (!window.confirm('Удалить фото блюда «' + name + '» из меню на все даты? Автоматическая замена не появится.')) return;
      }
      buttons.forEach(button => { button.disabled = true; });
      feedback.textContent = '';
      const query = new URLSearchParams(dish.photo_id ? { photo_id: dish.photo_id } : { image_url: dish.image_url });
      if (!removing) query.set('rotation', rotation);
      try {
        const response = await fetch('/api/menu/dish/' + encodeURIComponent(dish.id) + '/image?' + query, {
          method: removing ? 'DELETE' : 'PATCH', headers: { 'X-Telegram-Init-Data': initData },
        });
        if (response.status === 409) {
          feedback.textContent = 'Фото уже изменилось. Закройте карточку и обновите меню.';
          return;
        }
        if (response.status === 403) {
          feedback.textContent = 'Не удалось подтвердить права модератора. Откройте меню заново через бота.';
          return;
        }
        if (!response.ok) throw new Error('HTTP ' + response.status);
        if (removing) {
          if (dialog.open && openPhoto && sameImage(openPhoto.dish, dish)) dialog.close();
          load(requestedDate);
        } else {
          updateRotation(dish, rotation);
          feedback.textContent = 'Поворот сохранён.';
        }
      } catch {
        feedback.textContent = removing ? 'Не удалось удалить фото. Попробуйте ещё раз.' : 'Не удалось повернуть фото. Попробуйте ещё раз.';
      } finally {
        buttons.forEach(button => { button.disabled = false; });
      }
    }
    left.addEventListener('click', () => edit(((dish.image_rotation || 0) + 270) % 360));
    right.addEventListener('click', () => edit(((dish.image_rotation || 0) + 90) % 360));
    remove.addEventListener('click', () => edit());
    tools.append(...buttons);
    const panel = element('div', '', 'dish-image-panel');
    panel.append(tools, feedback);
    return panel;
  }
  function dishCard(dish) {
    const card = element('button', '', 'dish-card');
    card.type = 'button';
    const imageURL = safeURL(dish.image_url);
    const placeholder = () => {
      const node = element('div', '🍽', 'thumb thumb-placeholder');
      node.setAttribute('aria-hidden', 'true');
      return node;
    };
    if (imageURL && dish.image_status !== 'not_found') {
      const frame = element('div', '', 'thumb dish-thumb');
      const img = element('img');
      img.src = imageURL; img.alt = ''; img.loading = 'lazy'; img.referrerPolicy = 'no-referrer';
      img.onerror = () => frame.replaceWith(placeholder());
      frame.append(img);
      thumbnails.set(dish, frame);
      orientImage(frame, dish.image_rotation);
      card.append(frame);
    } else card.append(placeholder());
    const title = element('div');
    title.append(element('div', dish.translated_name || dish.name, 'dish-title'));
    if (dish.translated_name) title.append(element('div', dish.name, 'original'));
    if (dish.short_description) title.append(element('div', dish.short_description, 'description'));
    if (dish.image_kind === 'cafeteria') title.append(element('div', 'Фото из столовой', 'note'));
    card.append(title);
    card.addEventListener('click', () => openDish(dish));
    return card;
  }
  function openDish(dish) {
    dialogContent.replaceChildren();
    openPhoto = null;
    const imageURL = dish.image_status !== 'not_found' ? safeURL(dish.image_url) : '';
    if (imageURL) {
      const frame = element('div', '', 'dish-photo');
      const img = element('img', '', 'dialog-image');
      img.src = imageURL; img.alt = dish.image_kind === 'cafeteria' ? 'Фото блюда из столовой' : 'Пример изображения блюда'; img.referrerPolicy = 'no-referrer';
      img.onerror = () => frame.remove();
      orientImage(frame, dish.image_rotation);
      frame.append(img);
      openPhoto = { dish, frame };
      dialogContent.append(frame);
      if (photoAdmin) dialogContent.append(dishImageControls(dish));
    }
    const content = element('div', '', 'content');
    const heading = element('h2', dish.translated_name || dish.name);
    heading.id = 'dish-dialog-title';
    content.append(heading);
    if (dish.translated_name) content.append(element('p', dish.name, 'original'));
    if (dish.short_description) content.append(element('p', dish.short_description));
    if (dish.description) content.append(element('p', dish.description));
    if (dish.ingredients?.length) content.append(element('p', 'Типичные ингредиенты (не состав столовой): ' + dish.ingredients.join(', ')));
    if (dish.needs_review) content.append(element('p', 'Описание требует проверки.', 'note'));
    if (imageURL) {
      content.append(element('p', dish.image_kind === 'cafeteria' ? 'Одобренное фото блюда из столовой.' : 'Изображение — пример, может не соответствовать блюду в столовой' + (dish.image_status === 'needs_review' ? '; требует проверки.' : '.'), 'note'));
      if (dish.image_author) content.append(element('p', 'Автор: ' + dish.image_author, 'note'));
      addLink(content, 'Источник изображения', dish.image_source);
      if (dish.image_license) {
        const license = link('Лицензия: ' + dish.image_license, dish.image_license_url);
        const p = element('p'); p.append(license || document.createTextNode('Лицензия: ' + dish.image_license)); content.append(p);
      }
    }
    (dish.sources || []).forEach((source, i) => addLink(content, 'Источник описания ' + (i + 1), source));
    if (!dish.short_description && !dish.description && !dish.ingredients?.length && !imageURL && !dish.sources?.length) content.append(element('p', 'Подробности пока не подготовлены.', 'note'));
    dialogContent.append(content);
    dialog.showModal();
  }
  document.querySelector('#dish-close').addEventListener('click', () => dialog.close());
  dialog.addEventListener('click', event => {
    if (event.target !== dialog) return;
    const rect = dialog.getBoundingClientRect();
    if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) dialog.close();
  });
  function render(data) {
    mealsElement.replaceChildren();
    const order = ['BREAKFAST', 'LUNCH', 'DINNER'];
    const labels = { BREAKFAST: 'Завтрак', LUNCH: 'Обед', DINNER: 'Ужин' };
    const keys = Object.keys(data.meals).filter(key => data.meals[key].some(dish => dish.kind !== 'notice')).sort((a, b) => (order.indexOf(a) < 0 ? 3 : order.indexOf(a)) - (order.indexOf(b) < 0 ? 3 : order.indexOf(b)) || a.localeCompare(b));
    for (const key of keys) {
      const section = element('section'); section.append(element('h2', labels[key] || key));
      const groups = new Map();
      for (const dish of data.meals[key]) {
        if (dish.kind === 'notice') continue;
        const course = (dish.course || '').trim();
        if (!groups.has(course)) groups.set(course, []);
        groups.get(course).push(dish);
      }
      const courses = [...groups.keys()].sort((a, b) => !a ? 1 : !b ? -1 : a.localeCompare(b));
      for (const course of courses) {
        const group = element('div', '', 'course-group');
        group.append(element('h3', course ? 'Группа ' + course : 'Без группы'));
        groups.get(course).forEach(dish => group.append(dishCard(dish)));
        section.append(group);
      }
      mealsElement.append(section);
    }
    status.textContent = keys.length ? '' : 'На эту дату меню пока нет.';
  }
  async function load(value = '') {
    if (!initData) { status.textContent = 'Откройте меню через Telegram-бота.'; return; }
    requestedDate = value;
    request?.abort(); request = new AbortController();
    window.dispatchEvent(new Event('menu:loading'));
    status.textContent = 'Загрузка…'; mealsElement.replaceChildren();
    try {
      const response = await fetch('/api/menu' + (value ? '?date=' + encodeURIComponent(value) : ''), { headers: { 'X-Telegram-Init-Data': initData }, signal: request.signal, cache: 'no-store' });
      if (response.status === 401) { status.textContent = 'Не удалось подтвердить Telegram. Закройте и снова откройте меню через бота.'; return; }
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const data = await response.json(); date = data.date; photoAdmin = data.photo_admin; menuData = data;
      dateElement.textContent = new Intl.DateTimeFormat('ru-RU', { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric', timeZone: 'UTC' }).format(new Date(date + 'T12:00:00Z'));
      render(data);
      window.dispatchEvent(new CustomEvent('menu:loaded', { detail: data }));
    } catch (error) { if (error.name !== 'AbortError') status.textContent = 'Не удалось загрузить меню. Попробуйте ещё раз.'; }
  }
  function move(days) {
    if (!date) return;
    const d = new Date(date + 'T12:00:00Z'); d.setUTCDate(d.getUTCDate() + days);
    load(d.toISOString().slice(0, 10));
  }
  document.querySelector('#previous').addEventListener('click', () => move(-1));
  document.querySelector('#next').addEventListener('click', () => move(1));
  document.querySelector('#today').addEventListener('click', () => load(''));
  load();
})();
