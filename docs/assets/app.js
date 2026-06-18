/* ============================================================
   OmniCore docs — app logic
   Sections live in content/sections/<id>.html and load on demand.
   The sidebar + order come from content/nav.json.
   Must be served over http(s) (GitHub Pages, or a local server) —
   file:// blocks fetch(). For local preview: python3 -m http.server
   ============================================================ */
(function () {
    const html = document.documentElement;
    const contentCol = document.getElementById('contentCol');
    const navEl = document.getElementById('nav');
    const railEl = document.getElementById('rail');
    const pagerEl = document.getElementById('pager');
    const breadcrumbEl = document.getElementById('breadcrumb');
    const host = document.getElementById('sectionHost');
    const toastEl = document.getElementById('toast');

    /* ----- theme (applied early to avoid flash) ----- */
    const THEME_KEY = 'omnicore-theme';
    let theme = localStorage.getItem(THEME_KEY) || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    html.setAttribute('data-theme', theme);
    document.getElementById('themeBtn').addEventListener('click', () => {
        theme = html.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
        html.setAttribute('data-theme', theme);
        localStorage.setItem(THEME_KEY, theme);
    });

    /* ----- reading width ----- */
    const WIDTH_KEY = 'omnicore-width';
    html.setAttribute('data-width', localStorage.getItem(WIDTH_KEY) || 'cozy');

    /* ----- shared state ----- */
    let NAV = [], order = [], currentId = null, pendingMark = null;
    const labelOf = {}, groupOf = {};
    const loaded = new Map();          // id -> section element
    let preloadPromise = null;

    const COLLAPSE_KEY = 'omnicore-collapsed';
    let collapsedSet; try { collapsedSet = new Set(JSON.parse(localStorage.getItem(COLLAPSE_KEY) || '[]')); } catch (e) { collapsedSet = new Set(); }
    const saveCollapsed = () => localStorage.setItem(COLLAPSE_KEY, JSON.stringify([...collapsedSet]));

    let toastT;
    function showToast(msg) { toastEl.textContent = msg; toastEl.classList.add('show'); clearTimeout(toastT); toastT = setTimeout(() => toastEl.classList.remove('show'), 1600); }

    function scrollToEl(el, offset) { const top = el.getBoundingClientRect().top - contentCol.getBoundingClientRect().top + contentCol.scrollTop - (offset || 14); contentCol.scrollTo({ top: Math.max(0, top), behavior: 'smooth' }); }

    /* ----- syntax highlight (Go / YAML, restrained) ----- */
    const KW = new Set(('func package import type struct interface map chan go defer return if else for range switch case default break continue var const nil true false make new append len cap select fallthrough goto error string bool byte rune int int8 int16 int32 int64 uint uint8 uint16 uint32 uint64 uintptr float32 float64 context any').split(' '));
    function esc(s) { return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
    function highlight(code) {
        const text = code.textContent;
        const re = /(\/\/[^\n]*|#[^\n]*)|(`[^`]*`|"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*')|(\b\d[\d_.xa-fA-F]*\b)|([A-Za-z_]\w*)/g;
        let out = '', last = 0, m;
        while ((m = re.exec(text))) {
            out += esc(text.slice(last, m.index));
            if (m[1]) out += '<span class="tok-c">' + esc(m[1]) + '</span>';
            else if (m[2]) out += '<span class="tok-s">' + esc(m[2]) + '</span>';
            else if (m[3]) out += '<span class="tok-n">' + esc(m[3]) + '</span>';
            else out += KW.has(m[4]) ? '<span class="tok-k">' + esc(m[4]) + '</span>' : esc(m[4]);
            last = re.lastIndex;
        }
        out += esc(text.slice(last));
        code.innerHTML = out;
    }

    /* ----- enhance one freshly-loaded section ----- */
    function enhance(sec) {
        // ensure every h3 has an id (rail + copy-link anchors need it)
        const usedIds = new Set([...document.querySelectorAll('[id]')].map(e => e.id));
        sec.querySelectorAll('h3').forEach(h => {
            if (h.id) return;
            let base = (h.textContent || 'section').toLowerCase().normalize('NFD').replace(/[\u0300-\u036f]/g, '').replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 48) || 'section';
            let id = base, n = 2; while (usedIds.has(id)) id = base + '-' + (n++);
            usedIds.add(id); h.id = id;
        });
        // wrap tables for horizontal scroll
        sec.querySelectorAll('table').forEach(t => {
            if (t.closest('.table-wrap')) return;
            const w = document.createElement('div'); w.className = 'table-wrap';
            t.parentNode.insertBefore(w, t); w.appendChild(t);
        });
        // collapsible h3 groups (only when 2+ h3 directly in the section)
        const h3s = [...sec.children].filter(el => el.tagName === 'H3');
        if (h3s.length >= 2) {
            h3s.forEach(h3 => {
                const body = document.createElement('div'); body.className = 'collapse-body';
                let n = h3.nextElementSibling;
                while (n && n.tagName !== 'H3') { const nx = n.nextElementSibling; body.appendChild(n); n = nx; }
                h3.after(body);
                const chev = document.createElement('span'); chev.className = 'chev'; chev.textContent = '\u25be';
                h3.insertBefore(chev, h3.firstChild);
                h3.classList.add('collapsible'); h3.tabIndex = 0; h3.setAttribute('role', 'button');
                if (h3.id && collapsedSet.has(h3.id)) { h3.classList.add('collapsed'); body.style.display = 'none'; }
                const toggle = (e) => {
                    if (e && e.target.closest('a')) return;
                    const c = h3.classList.toggle('collapsed');
                    body.style.display = c ? 'none' : '';
                    if (h3.id) { c ? collapsedSet.add(h3.id) : collapsedSet.delete(h3.id); saveCollapsed(); }
                };
                h3.addEventListener('click', toggle);
                h3.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(e); } });
            });
        }
        // copy-link anchors on every h3
        sec.querySelectorAll('h3[id]').forEach(h => {
            const a = document.createElement('a'); a.className = 'h3-anchor'; a.href = '#' + sec.id + '~' + h.id;
            a.title = 'Copiar link'; a.setAttribute('aria-label', 'Copiar link da subse\u00e7\u00e3o');
            a.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M10 13a5 5 0 0 0 7 0l3-3a5 5 0 0 0-7-7l-1 1"/><path d="M14 11a5 5 0 0 0-7 0l-3 3a5 5 0 0 0 7 7l1-1"/></svg>';
            a.addEventListener('click', e => {
                e.preventDefault(); e.stopPropagation();
                const url = location.origin + location.pathname + '#' + sec.id + '~' + h.id;
                if (navigator.clipboard) navigator.clipboard.writeText(url).catch(() => {});
                history.replaceState(null, '', '#' + sec.id + '~' + h.id);
                a.classList.add('done'); setTimeout(() => a.classList.remove('done'), 1200);
                showToast('Link copiado');
            });
            h.appendChild(a);
        });
        // syntax highlight + copy buttons
        sec.querySelectorAll('pre').forEach(pre => {
            const code = pre.querySelector('code');
            if (code) highlight(code);
            const wrap = document.createElement('div'); wrap.className = 'code-wrap';
            pre.parentNode.insertBefore(wrap, pre); wrap.appendChild(pre);
            const btn = document.createElement('button'); btn.className = 'copy-btn'; btn.type = 'button'; btn.textContent = 'Copy';
            btn.addEventListener('click', () => {
                navigator.clipboard.writeText((code || pre).textContent).then(() => {
                    btn.textContent = 'Copied'; btn.classList.add('done');
                    setTimeout(() => { btn.textContent = 'Copy'; btn.classList.remove('done'); }, 1400);
                });
            });
            wrap.appendChild(btn);
        });
        // eyebrow above the h2 (skip overview, which has the hero)
        if (!sec.querySelector('.hero')) {
            const h2 = sec.querySelector('h2');
            if (h2 && !h2.previousElementSibling) {
                const eb = document.createElement('div'); eb.className = 'eyebrow'; eb.textContent = groupOf[sec.id] || '';
                sec.insertBefore(eb, h2);
            }
        }
    }

    /* ----- fetch + cache a section ----- */
    async function loadSection(id) {
        if (loaded.has(id)) return loaded.get(id);
        const sec = document.createElement('section');
        sec.id = id; sec.className = 'doc-section';
        try {
            const res = await fetch('content/sections/' + id + '.html');
            if (!res.ok) throw new Error(res.status);
            sec.innerHTML = await res.text();
            enhance(sec);
        } catch (e) {
            sec.innerHTML = '<h2>Couldn\u2019t load this section</h2><p class="muted">Tried <code>content/sections/' + id + '.html</code>. If you opened the file directly, serve it over http instead (e.g. <code>python3 -m http.server</code>) \u2014 browsers block <code>fetch()</code> on <code>file://</code>.</p>';
        }
        host.appendChild(sec);
        loaded.set(id, sec);
        return sec;
    }

    function preloadAll() {
        if (!preloadPromise) preloadPromise = Promise.all(order.map(loadSection));
        return preloadPromise;
    }

    /* ----- search marks ----- */
    function clearMarks() { document.querySelectorAll('mark.search-hit').forEach(m => { const t = document.createTextNode(m.textContent); m.replaceWith(t); }); loaded.forEach(s => s.normalize()); }
    function markTerm(sec, q) {
        if (!q) return;
        const rx = new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'gi');
        const walker = document.createTreeWalker(sec, NodeFilter.SHOW_TEXT, { acceptNode(n) { if (!n.nodeValue.trim()) return NodeFilter.FILTER_REJECT; rx.lastIndex = 0; return rx.test(n.nodeValue) ? NodeFilter.FILTER_ACCEPT : NodeFilter.FILTER_REJECT; } });
        const nodes = []; while (walker.nextNode()) nodes.push(walker.currentNode);
        nodes.forEach(n => {
            const s = n.nodeValue, frag = document.createDocumentFragment(); let last = 0, m; rx.lastIndex = 0;
            while ((m = rx.exec(s))) { frag.appendChild(document.createTextNode(s.slice(last, m.index))); const mk = document.createElement('mark'); mk.className = 'search-hit'; mk.textContent = m[0]; frag.appendChild(mk); last = m.index + m[0].length; if (m.index === rx.lastIndex) rx.lastIndex++; }
            frag.appendChild(document.createTextNode(s.slice(last))); n.replaceWith(frag);
        });
    }

    /* ----- on this page ----- */
    let railLinks = [];
    function buildRail(sec) {
        railEl.innerHTML = '';
        const h3s = [...sec.querySelectorAll('h3')].filter(h => h.id);
        if (!h3s.length) { railEl.innerHTML = '<div class="empty">\u2014</div>'; railLinks = []; return; }
        railLinks = h3s.map(h => {
            const a = document.createElement('a'); a.href = '#' + sec.id;
            a.textContent = h.textContent.replace(/^\u25be\s*/, '');
            a.addEventListener('click', e => { e.preventDefault(); scrollToEl(h, 64); });
            railEl.appendChild(a);
            return { a, h };
        });
    }
    function spyRail() {
        if (!railLinks.length) return;
        const base = contentCol.getBoundingClientRect().top + 90;
        let cur = railLinks[0];
        for (const l of railLinks) { if (l.h.getBoundingClientRect().top <= base) cur = l; }
        railLinks.forEach(l => l.a.classList.toggle('active', l === cur));
    }

    /* ----- prev / next ----- */
    function buildPager(id) {
        const i = order.indexOf(id);
        const prev = i > 0 ? order[i - 1] : null;
        const next = i < order.length - 1 ? order[i + 1] : null;
        const cell = (tid, dir, cls) => tid
            ? '<a class="' + cls + '" href="#' + tid + '"><div class="dir">' + dir + '</div><div class="ttl">' + (labelOf[tid] || tid) + '</div></a>'
            : '<a class="' + cls + ' placeholder"></a>';
        pagerEl.innerHTML = cell(prev, '\u2190 Previous', 'prev') + cell(next, 'Next \u2192', 'next');
    }

    function setBreadcrumb(id) {
        breadcrumbEl.innerHTML = '<span class="crumb-group">' + (groupOf[id] || 'Docs') + '</span><span class="sep">/</span><span class="crumb-page">' + (labelOf[id] || id) + '</span>';
    }

    /* ----- activate a section ----- */
    const SEC_KEY = 'omnicore-section';
    let activateToken = 0;
    async function activate(id, sub) {
        if (!order.includes(id)) id = order[0];
        const token = ++activateToken;
        currentId = id;
        clearMarks();
        navEl.querySelectorAll('.nav-link').forEach(a => a.classList.toggle('active', a.dataset.id === id));
        setBreadcrumb(id); buildPager(id);
        localStorage.setItem(SEC_KEY, id);
        document.body.classList.remove('nav-open');
        document.title = (labelOf[id] || 'OmniCore') + ' \u2014 OmniCore Docs';

        const sec = await loadSection(id);
        if (token !== activateToken) return; // a newer navigation won
        loaded.forEach(s => s.classList.toggle('active', s === sec));
        buildRail(sec);

        if (pendingMark) {
            sec.querySelectorAll('h3.collapsed').forEach(h => { h.classList.remove('collapsed'); const b = h.nextElementSibling; if (b && b.classList.contains('collapse-body')) b.style.display = ''; });
            markTerm(sec, pendingMark);
            const first = sec.querySelector('mark.search-hit');
            requestAnimationFrame(() => { if (first) scrollToEl(first, 96); else contentCol.scrollTo({ top: 0 }); spyRail(); });
            pendingMark = null;
        } else if (sub) {
            const h = document.getElementById(sub);
            if (h && h.closest('.doc-section') === sec) {
                if (h.classList.contains('collapsed')) { h.classList.remove('collapsed'); const b = h.nextElementSibling; if (b && b.classList.contains('collapse-body')) b.style.display = ''; }
                requestAnimationFrame(() => { scrollToEl(h, 64); spyRail(); });
            } else { contentCol.scrollTo({ top: 0 }); spyRail(); }
        } else {
            contentCol.scrollTo({ top: 0 }); spyRail();
        }
    }

    /* ----- routing (#section or #section~subsection) ----- */
    function parseHash() {
        const raw = decodeURIComponent((location.hash || '').replace(/^#/, ''));
        if (!raw) return { sec: '', sub: '' };
        const i = raw.indexOf('~');
        return i === -1 ? { sec: raw, sub: '' } : { sec: raw.slice(0, i), sub: raw.slice(i + 1) };
    }
    function route() {
        const { sec, sub } = parseHash();
        if (sec && !order.includes(sec)) { showToast('Se\u00e7\u00e3o n\u00e3o encontrada'); activate(order[0]); return; }
        activate(sec || order[0], sub);
    }
    window.addEventListener('hashchange', route);

    /* ----- sidebar nav ----- */
    function buildNav() {
        NAV.forEach(g => {
            const grp = document.createElement('div'); grp.className = 'nav-group'; grp.dataset.group = g.group;
            const t = document.createElement('div'); t.className = 'nav-group-title'; t.textContent = g.group; grp.appendChild(t);
            g.items.forEach(it => {
                const a = document.createElement('a'); a.href = '#' + it.id; a.className = 'nav-link'; a.dataset.id = it.id;
                a.innerHTML = it.label;
                grp.appendChild(a);
            });
            navEl.appendChild(grp);
        });
        const noResult = document.createElement('div'); noResult.className = 'nav-empty'; noResult.textContent = 'No matches'; noResult.style.display = 'none';
        navEl.appendChild(noResult);
        return noResult;
    }

    /* ----- search ----- */
    function wireSearch(noResult) {
        const searchInput = document.getElementById('search');
        let index = null; // [{id,label,text}]
        async function ensureIndex() {
            if (index) return index;
            await preloadAll();
            index = order.map(id => ({ id, label: (labelOf[id] || id).toLowerCase(), text: (loaded.get(id)?.textContent || '').toLowerCase() }));
            return index;
        }
        function applyFilter(matchSet) {
            const links = navEl.querySelectorAll('.nav-link');
            links.forEach(a => a.style.display = (!matchSet || matchSet.has(a.dataset.id)) ? '' : 'none');
            navEl.querySelectorAll('.nav-group').forEach(g => {
                const any = [...g.querySelectorAll('.nav-link')].some(a => a.style.display !== 'none');
                g.style.display = any ? '' : 'none';
            });
            noResult.style.display = (matchSet && matchSet.size === 0) ? '' : 'none';
        }
        searchInput.addEventListener('input', async () => {
            const q = searchInput.value.trim().toLowerCase();
            if (!q) { applyFilter(null); return; }
            const idx = await ensureIndex();
            const match = new Set(idx.filter(h => h.label.includes(q) || h.text.includes(q)).map(h => h.id));
            applyFilter(match);
        });
        searchInput.addEventListener('keydown', e => {
            if (e.key === 'Enter') {
                const q = searchInput.value.trim();
                const first = [...navEl.querySelectorAll('.nav-link')].find(a => a.style.display !== 'none');
                if (first) { pendingMark = q || null; if ('#' + first.dataset.id === location.hash) route(); else location.hash = '#' + first.dataset.id; searchInput.blur(); }
            }
            if (e.key === 'Escape') { searchInput.value = ''; applyFilter(null); clearMarks(); searchInput.blur(); }
        });
        document.addEventListener('keydown', e => {
            const typing = /input|textarea/i.test(document.activeElement.tagName);
            if (e.key === '/' && !typing) { e.preventDefault(); searchInput.focus(); return; }
            if (e.key === 'Escape') document.body.classList.remove('nav-open');
            if (typing || e.metaKey || e.ctrlKey || e.altKey) return;
            const i = order.indexOf(currentId);
            if (e.key === 'ArrowRight' && i < order.length - 1) location.hash = '#' + order[i + 1];
            if (e.key === 'ArrowLeft' && i > 0) location.hash = '#' + order[i - 1];
        });
    }

    /* ----- width + print + mobile ----- */
    function wireChrome() {
        document.getElementById('widthBtn').addEventListener('click', () => {
            const w = html.getAttribute('data-width') === 'wide' ? 'cozy' : 'wide';
            html.setAttribute('data-width', w); localStorage.setItem(WIDTH_KEY, w);
            showToast(w === 'wide' ? 'Largura ampla' : 'Largura confort\u00e1vel');
        });
        document.getElementById('printBtn').addEventListener('click', async () => {
            showToast('Preparando impress\u00e3o\u2026');
            await preloadAll();
            requestAnimationFrame(() => window.print());
        });
        let prePrintTheme;
        window.addEventListener('beforeprint', () => { prePrintTheme = html.getAttribute('data-theme'); html.setAttribute('data-theme', 'light'); loaded.forEach(s => s.classList.add('active')); });
        window.addEventListener('afterprint', () => { html.setAttribute('data-theme', prePrintTheme || 'light'); loaded.forEach(s => s.classList.toggle('active', s.id === currentId)); });

        document.getElementById('hamburger').addEventListener('click', () => document.body.classList.toggle('nav-open'));
        document.getElementById('scrim').addEventListener('click', () => document.body.classList.remove('nav-open'));

        let ticking = false;
        contentCol.addEventListener('scroll', () => { if (ticking) return; ticking = true; requestAnimationFrame(() => { spyRail(); ticking = false; }); });
    }

    /* ----- boot ----- */
    (async function boot() {
        try {
            const res = await fetch('content/nav.json');
            NAV = await res.json();
        } catch (e) {
            host.innerHTML = '<h2>Couldn\u2019t load the navigation</h2><p class="muted">Tried <code>content/nav.json</code>. Serve this over http instead of opening the file directly \u2014 e.g. <code>python3 -m http.server</code> \u2014 since browsers block <code>fetch()</code> on <code>file://</code>.</p>';
            return;
        }
        order = NAV.flatMap(g => g.items.map(i => i.id));
        NAV.forEach(g => g.items.forEach(it => { labelOf[it.id] = it.label; groupOf[it.id] = g.group; }));

        const noResult = buildNav();
        wireSearch(noResult);
        wireChrome();

        if (location.hash) route();
        else { const start = localStorage.getItem(SEC_KEY) || order[0]; history.replaceState(null, '', '#' + start); activate(start); }

        // warm the cache in the background so search + print + nav feel instant
        if ('requestIdleCallback' in window) requestIdleCallback(() => preloadAll());
        else setTimeout(() => preloadAll(), 1200);
    })();
})();
