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
  let date = '', request;

  const element = (tag, text, className) => {
    const node = document.createElement(tag);
    if (text) node.textContent = text;
    if (className) node.className = className;
    return node;
  };
  const safeURL = value => {
    try { const url = new URL(value); return url.protocol === 'https:' ? url.href : ''; } catch { return ''; }
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
  function dishCard(dish) {
    const card = element('button', '', 'dish-card');
    card.type = 'button';
    const imageURL = safeURL(dish.image_url);
    if (imageURL && dish.image_status !== 'not_found') {
      const img = element('img', '', 'thumb');
      img.src = imageURL; img.alt = ''; img.loading = 'lazy'; img.referrerPolicy = 'no-referrer';
      img.onerror = () => img.remove();
      card.append(img);
    }
    const title = element('div');
    title.append(element('div', dish.translated_name || dish.name, 'dish-title'));
    if (dish.translated_name) title.append(element('div', dish.name, 'original'));
    if (dish.short_description) title.append(element('div', dish.short_description, 'description'));
    if (dish.course) title.append(element('div', dish.course, 'note'));
    card.append(title);
    card.addEventListener('click', () => openDish(dish));
    return card;
  }
  function openDish(dish) {
    dialogContent.replaceChildren();
    const imageURL = dish.image_status !== 'not_found' ? safeURL(dish.image_url) : '';
    if (imageURL) {
      const img = element('img', '', 'dialog-image');
      img.src = imageURL; img.alt = 'Пример изображения блюда'; img.referrerPolicy = 'no-referrer';
      img.onerror = () => img.remove();
      dialogContent.append(img);
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
      content.append(element('p', 'Изображение — пример, может не соответствовать блюду в столовой' + (dish.image_status === 'needs_review' ? '; требует проверки.' : '.'), 'note'));
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
    const keys = Object.keys(data.meals).sort((a, b) => (order.indexOf(a) < 0 ? 3 : order.indexOf(a)) - (order.indexOf(b) < 0 ? 3 : order.indexOf(b)) || a.localeCompare(b));
    for (const key of keys) {
      const section = element('section'); section.append(element('h2', labels[key] || key));
      data.meals[key].forEach(dish => section.append(dishCard(dish)));
      mealsElement.append(section);
    }
    status.textContent = keys.length ? '' : 'На эту дату меню пока нет.';
  }
  async function load(value = '') {
    if (!initData) { status.textContent = 'Откройте меню через Telegram-бота.'; return; }
    request?.abort(); request = new AbortController();
    status.textContent = 'Загрузка…'; mealsElement.replaceChildren();
    try {
      const response = await fetch('/api/menu' + (value ? '?date=' + encodeURIComponent(value) : ''), { headers: { 'X-Telegram-Init-Data': initData }, signal: request.signal, cache: 'no-store' });
      if (response.status === 401) { status.textContent = 'Не удалось подтвердить Telegram. Закройте и снова откройте меню через бота.'; return; }
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const data = await response.json(); date = data.date;
      dateElement.textContent = new Intl.DateTimeFormat('ru-RU', { weekday: 'long', day: 'numeric', month: 'long', year: 'numeric', timeZone: 'UTC' }).format(new Date(date + 'T12:00:00Z'));
      render(data);
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
