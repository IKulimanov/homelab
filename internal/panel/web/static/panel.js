// Панель работает и без скрипта: формы отправляются обычным POST. Скрипт добавляет удобства.
(function () {
  'use strict';
  const $ = (s, root) => (root || document).querySelector(s);
  const $$ = (s, root) => Array.from((root || document).querySelectorAll(s));

  function post(url, data) {
    return fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams(data),
      credentials: 'same-origin',
    });
  }

  // Тост: скрыть через 6 секунд и убрать ?toast из адреса, чтобы обновление страницы не показывало его снова.
  const toast = $('.toast');
  function showToast(text) {
    toast.textContent = text;
    toast.hidden = false;
    clearTimeout(showToast.t);
    showToast.t = setTimeout(() => { toast.hidden = true; }, 6000);
  }
  if (toast && !toast.hidden) showToast(toast.textContent);
  const u = new URL(location.href);
  if (u.searchParams.has('toast')) {
    u.searchParams.delete('toast');
    history.replaceState(null, '', u.pathname + (u.search || '') + u.hash);
  }

  // Подтверждение: форма с data-confirm отправляется только после «Да».
  const dlg = $('#confirm');
  $$('form[data-confirm]').forEach((f) => {
    f.addEventListener('submit', (e) => {
      if (f.dataset.confirmed === '1' || !dlg || !dlg.showModal) return;
      e.preventDefault();
      $('[data-title]', dlg).textContent = f.dataset.confirmTitle || 'Точно?';
      $('[data-text]', dlg).textContent = f.dataset.confirm;
      dlg.returnValue = '';
      dlg.onclose = () => {
        if (dlg.returnValue === 'ok') {
          f.dataset.confirmed = '1';
          f.requestSubmit ? f.requestSubmit() : f.submit();
        }
      };
      dlg.showModal();
    });
  });

  // На телефоне меню прокручивается вбок: показать активный пункт.
  const navOn = $('.nav-items a.on');
  if (navOn && navOn.parentElement.scrollWidth > navOn.parentElement.clientWidth) {
    navOn.parentElement.scrollLeft = navOn.offsetLeft - 16;
  }

  $$('select[data-autosubmit]').forEach((s) => s.addEventListener('change', () => s.form.submit()));

  // Гипножаба: пять кликов по кораблю или код Konami.
  const toad = $('.toad');
  function hail() { if (toad) toad.hidden = false; }
  if (toad) $('[data-wake]', toad).addEventListener('click', () => { toad.hidden = true; });
  const konami = ['ArrowUp', 'ArrowUp', 'ArrowDown', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'ArrowLeft', 'ArrowRight', 'b', 'a'];
  let kpos = 0;
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && toad) toad.hidden = true;
    kpos = e.key === konami[kpos] ? kpos + 1 : (e.key === konami[0] ? 1 : 0);
    if (kpos === konami.length) { kpos = 0; hail(); }
  });

  const poke = $('[data-poke]');
  if (poke) {
    let clicks = 0;
    const bubble = $('[data-bubble]');
    poke.addEventListener('click', async () => {
      clicks++;
      if (clicks % 5 === 0) hail();
      try {
        const r = await fetch('/api/poke' + (poke.hasAttribute('data-bad') ? '?bad=1' : ''), { credentials: 'same-origin' });
        const j = await r.json();
        bubble.textContent = j.text;
        bubble.classList.remove('pop');
        void bubble.offsetWidth;
        bubble.classList.add('pop');
      } catch (_) { /* фраза не обязательна */ }
    });
  }

  // Ожидание update.sh: опрос раз в 5 секунд, по окончании — перезагрузка страницы с итогом.
  if ($('[data-poll-run]')) {
    const timer = setInterval(async () => {
      try {
        const r = await fetch('/api/run', { credentials: 'same-origin' });
        const j = await r.json();
        if (!j.running) { clearInterval(timer); location.reload(); }
      } catch (_) { /* следующая попытка */ }
    }, 5000);
  }

  // Бортовой журнал: поток SSE, фильтр, пауза.
  const term = $('[data-log-src]');
  if (term) {
    const filter = $('[data-log-filter]');
    const pause = $('[data-log-pause]');
    const state = $('[data-log-state]');
    const max = 5000;
    let paused = false;
    let buffer = [];
    term.textContent = '';
    function matches(line) {
      const q = filter.value.trim().toLowerCase();
      return !q || line.toLowerCase().includes(q);
    }
    function add(line) {
      const el = document.createElement('div');
      el.textContent = line;
      if (/\b(error|fatal|panic|ошибка|упал)/i.test(line)) el.className = 'err';
      else if (/\b(warn|warning|предупрежд)/i.test(line)) el.className = 'wrn';
      el.hidden = !matches(line);
      term.appendChild(el);
      while (term.childElementCount > max) term.firstElementChild.remove();
    }
    function flush() {
      const stick = term.scrollTop + term.clientHeight >= term.scrollHeight - 40;
      buffer.forEach(add);
      buffer = [];
      if (stick) term.scrollTop = term.scrollHeight;
    }
    const es = new EventSource(term.dataset.logSrc);
    es.onmessage = (e) => { buffer.push(e.data); if (!paused) flush(); };
    es.addEventListener('end', () => { es.close(); state.textContent = 'Поток закрыт: контейнер остановился или перезапустился. Обнови страницу.'; });
    es.onerror = () => { state.textContent = 'Связь с потоком прервалась, пробую снова…'; };
    es.onopen = () => { state.textContent = 'Поток идёт в реальном времени.'; };
    filter.addEventListener('input', () => {
      $$('div', term).forEach((el) => { el.hidden = !matches(el.textContent); });
    });
    pause.addEventListener('click', () => {
      paused = !paused;
      pause.textContent = paused ? 'Продолжить' : 'Пауза';
      if (!paused) flush();
    });
  }

  // Экипаж: CPU и память приходят отдельным запросом. Docker замеряет их около секунды, страница их не ждёт.
  const res = $$('[data-res]');
  if (res.length) {
    fetch('/api/crew/stats', { credentials: 'same-origin' })
      .then((r) => (r.ok ? r.json() : {}))
      .catch(() => ({}))
      .then((stats) => res.forEach((el) => { el.textContent = stats[el.dataset.res] || '—'; }));
  }

  // Сейф: отметка «поле тронуто» и показ значения по запросу.
  $$('input[data-key]').forEach((inp) => {
    inp.addEventListener('input', () => { inp.form.elements['t_' + inp.dataset.key].value = '1'; });
  });
  $$('[data-reveal]').forEach((btn) => {
    btn.addEventListener('click', async () => {
      const shown = $('[data-shown]', btn.parentElement);
      if (!shown.hidden) { shown.hidden = true; shown.textContent = ''; btn.textContent = 'Показать'; return; }
      try {
        const r = await post('/vault/' + encodeURIComponent(btn.dataset.file) + '/reveal', { key: btn.dataset.reveal });
        if (!r.ok) throw new Error(await r.text());
        const j = await r.json();
        shown.textContent = j.value === '' ? '(пусто)' : j.value;
        shown.hidden = false;
        btn.textContent = 'Скрыть';
        setTimeout(() => { shown.hidden = true; shown.textContent = ''; btn.textContent = 'Показать'; }, 30000);
      } catch (e) { showToast('Не показать: ' + e.message); }
    });
  });

  // Лаборатория: запрос без перезагрузки страницы, таблица результата.
  const sqlForm = $('form[data-sql]');
  if (sqlForm) {
    const out = $('[data-result]');
    const area = sqlForm.elements.sql;
    async function run() {
      out.innerHTML = '<p class="muted cursor">Профессор думает</p>';
      try {
        const r = await post('/lab/query', { db: sqlForm.elements.db.value, sql: area.value });
        const j = await r.json();
        render(j);
      } catch (e) { out.innerHTML = ''; showToast('Запрос не дошёл: ' + e.message); }
    }
    function render(j) {
      out.textContent = '';
      const info = document.createElement('p');
      info.className = 'muted small';
      if (j.error) {
        const n = document.createElement('div');
        n.className = 'note bad';
        n.textContent = j.error;
        out.appendChild(n);
        return;
      }
      if (j.write) {
        const n = document.createElement('div');
        n.className = 'note ok';
        n.textContent = 'Хорошие новости, все! Изменено строк: ' + j.changed + '. Запись ушла в журнал и в Telegram.';
        out.appendChild(n);
        return;
      }
      info.textContent = 'Строк: ' + j.rows.length + (j.truncated ? ' (показаны первые, остальные — в CSV)' : '') + ' · ' + j.ms + ' мс';
      out.appendChild(info);
      if (!j.columns || j.columns.length === 0) return;
      const wrap = document.createElement('div');
      wrap.className = 'scroll';
      wrap.style.maxHeight = '60vh';
      const t = document.createElement('table');
      t.className = 'tbl';
      const hr = t.createTHead().insertRow();
      j.columns.forEach((c) => { const th = document.createElement('th'); th.textContent = c; hr.appendChild(th); });
      const body = t.createTBody();
      j.rows.forEach((row) => {
        const tr = body.insertRow();
        row.forEach((v) => {
          const td = tr.insertCell();
          if (v === null) { td.textContent = 'NULL'; td.className = 'null'; } else td.textContent = String(v);
        });
      });
      wrap.appendChild(t);
      out.appendChild(wrap);
    }
    $('[data-run]', sqlForm).addEventListener('click', (e) => { e.preventDefault(); run(); });
    area.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); run(); }
    });
    $$('[data-table]').forEach((a) => a.addEventListener('click', (e) => {
      e.preventDefault();
      area.value = 'SELECT * FROM "' + a.dataset.table.replace(/"/g, '""') + '" LIMIT 100';
      run();
    }));
  }
})();
