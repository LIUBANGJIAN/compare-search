/*
 * 多引擎检索面板的前端逻辑。
 *
 * 本文件既会被 //go:embed 内联进面板页，也可在 Node 下 require 做单元测试，
 * 所以刻意把纯函数（番号切分 / URL 构造 / 翻页索引 / 槽位选择 / 宽度夹取）
 * 与 DOM 初始化分开。
 *
 * 每个引擎有两个 iframe「槽位」，轮流担任「正在显示」与「预取下一条」：
 * 用户点下一条时，如果另一个槽位已经把这条加载好了，直接切可见性即可瞬时呈现，
 * 而不是等到点了才去请求。
 */
(function (root, factory) {
    'use strict';
    var api = factory();
    if (typeof module !== 'undefined' && module.exports) {
        module.exports = api;
    }
    if (typeof document !== 'undefined' && root) {
        root.__panelApi = api;
        if (document.readyState === 'loading') {
            document.addEventListener('DOMContentLoaded', function () { api.init(); });
        } else {
            api.init();
        }
    }
})(typeof globalThis !== 'undefined' ? globalThis : this, function () {
    'use strict';

    // 番号分隔符：换行（含 CRLF）、英文/中文逗号、英文/中文分号、顿号。
    // 刻意不把「空格」算作分隔符 —— 一个番号就是在输入框里的一行。
    var SEPARATOR = /[\r\n]+|[,，;；、]+/;

    var DEFAULT_CODES_WIDTH = 216;
    var MIN_CODES_WIDTH = 150;
    var MAX_CODES_WIDTH = 640;
    var BLANK = 'about:blank';

    /**
     * 把用户输入切分成番号列表：去空白、去重、保持原始顺序。
     * @param {*} raw 用户输入
     * @returns {string[]}
     */
    function splitKeywords(raw) {
        if (raw === null || raw === undefined) return [];
        var parts = String(raw).split(SEPARATOR);
        var out = [];
        var seen = Object.create(null);
        for (var i = 0; i < parts.length; i++) {
            var k = parts[i].trim();
            if (k === '' || seen[k]) continue;
            seen[k] = true;
            out.push(k);
        }
        return out;
    }

    /**
     * 生成某个引擎对某个番号的检索地址。
     * @param {object} engine   {name, base, path, proxyPort}
     * @param {string} keyword  番号
     * @param {string} proto    页面协议，如 http:
     * @param {string} hostname 页面主机名（用主机名而不是 127.0.0.1，局域网访问时才正确）
     * @returns {string}
     */
    function engineUrl(engine, keyword, proto, hostname) {
        if (!engine || !engine.proxyPort) return BLANK;
        var path = engine.path || '/';
        if (path.charAt(0) !== '/') path = '/' + path;
        return String(proto) + '//' + String(hostname) + ':' + engine.proxyPort +
            path + '?q=' + encodeURIComponent(keyword);
    }

    /**
     * 决定这次该用哪个槽位呈现：
     * 另一个槽位若已经加载好了同一个番号的同一个地址，就切过去（瞬时）；
     * 否则仍用当前槽位重新加载。
     * @param {{active:number, loaded:number[], urls:string[]}} slot
     * @param {number} index 番号下标
     * @param {string} url   该番号在当前引擎配置下应有的地址
     * @returns {number} 槽位下标（0 或 1）
     */
    function chooseSlot(slot, index, url) {
        var other = 1 - slot.active;
        if (slot.loaded[other] === index && slot.urls[other] === url) {
            return other;
        }
        return slot.active;
    }

    /** 下一个索引；已在末尾则原地不动。index = -1 表示还没开始检索。 */
    function nextIndex(index, total) {
        if (total > 0 && index + 1 < total) return index + 1;
        return index;
    }

    /** 上一个索引；已在开头（或尚未开始）则原地不动。 */
    function prevIndex(index, total) {
        if (total > 0 && index - 1 >= 0) return index - 1;
        return index;
    }

    /** 番号在列表中的位置；找不到返回 -1。 */
    function indexOfKeyword(items, keyword) {
        for (var i = 0; i < items.length; i++) {
            if (items[i] === keyword) return i;
        }
        return -1;
    }

    /** 把番号栏宽度夹到允许区间内。 */
    function clampCodesWidth(px) {
        var n = parseInt(px, 10);
        if (isNaN(n)) return DEFAULT_CODES_WIDTH;
        if (n < MIN_CODES_WIDTH) return MIN_CODES_WIDTH;
        if (n > MAX_CODES_WIDTH) return MAX_CODES_WIDTH;
        return n;
    }

    function init() {
        var d = document;
        var els = {
            resultsCol: d.getElementById('resultsCol'),
            kwInput: d.getElementById('kwInput'),
            searchBtn: d.getElementById('searchBtn'),
            settingsBtn: d.getElementById('settingsBtn'),
            prevBtn: d.getElementById('prevBtn'),
            nextBtn: d.getElementById('nextBtn'),
            pager: d.getElementById('pager'),
            pagerInfo: d.getElementById('pagerInfo'),
            pagerKw: d.getElementById('pagerKw'),
            clearBtn: d.getElementById('clearBtn'),
            listBox: d.getElementById('listBox'),
            codesCount: d.getElementById('codesCount'),
            codesToggle: d.getElementById('codesToggle'),
            layout: d.getElementById('layout'),
            splitter: d.getElementById('splitter'),
            dialog: d.getElementById('settingsDialog'),
            directLink: d.getElementById('directLink'),
            engineList: d.getElementById('engineList'),
            addEngineBtn: d.getElementById('addEngineBtn'),
            settingsHint: d.getElementById('settingsHint'),
            settingsSave: d.getElementById('settingsSave'),
            settingsCancel: d.getElementById('settingsCancel')
        };
        for (var name in els) {
            if (Object.prototype.hasOwnProperty.call(els, name) && !els[name]) return;
        }

        // ---------------- 引导数据 ----------------
        var boot = {
            items: [], engines: [], origin: '', version: '', q: '', maxEngines: 8
        };
        var bootEl = d.getElementById('bootstrap');
        if (bootEl && bootEl.textContent) {
            try {
                var parsed = JSON.parse(bootEl.textContent);
                if (parsed && typeof parsed === 'object') {
                    if (Array.isArray(parsed.items)) boot.items = parsed.items;
                    if (Array.isArray(parsed.engines)) boot.engines = parsed.engines;
                    if (typeof parsed.origin === 'string') boot.origin = parsed.origin;
                    if (typeof parsed.version === 'string') boot.version = parsed.version;
                    if (typeof parsed.q === 'string') boot.q = parsed.q;
                    if (typeof parsed.maxEngines === 'number') boot.maxEngines = parsed.maxEngines;
                }
            } catch (e) {
                /* 引导数据损坏时退化为空列表，页面仍可用 */
            }
        }

        var proto = window.location.protocol;
        var hostname = window.location.hostname;
        var storageKey = 't2h_' + (window.location.port || 'default');

        var state = {
            items: [],
            index: -1,
            viewed: Object.create(null),
            engines: boot.engines.slice(),
            origin: boot.origin,
            maxEngines: boot.maxEngines || 8
        };

        function readStore(key, fallback) {
            try {
                var v = localStorage.getItem(storageKey + key);
                return v === null ? fallback : v;
            } catch (e) {
                return fallback;
            }
        }

        function writeStore(key, value) {
            try {
                localStorage.setItem(storageKey + key, value);
            } catch (e) {
                /* 隐私模式下写失败可忽略 */
            }
        }

        try {
            var saved = JSON.parse(readStore('_viewed', '{}'));
            if (saved && typeof saved === 'object' && !Array.isArray(saved)) {
                for (var vk in saved) {
                    if (Object.prototype.hasOwnProperty.call(saved, vk)) state.viewed[vk] = true;
                }
            }
        } catch (e) {
            state.viewed = Object.create(null);
        }

        // ---------------- 结果面板（每个引擎一个，含两个槽位） ----------------
        var slots = [];

        function buildEnginePanels() {
            // 原地清空而不是重新赋值：这样外部持有的 slots 引用（state.slots）不会失效
            slots.length = 0;
            els.resultsCol.textContent = '';
            if (state.engines.length === 0) {
                var tip = d.createElement('div');
                tip.classList.add('list-empty');
                tip.textContent = '还没有配置搜索引擎。点右上角「设置」添加。';
                els.resultsCol.appendChild(tip);
                return;
            }
            state.engines.forEach(function (eng, i) {
                var panel = d.createElement('div');
                panel.classList.add('panel');

                var header = d.createElement('div');
                header.classList.add('panel-header');
                var title = d.createElement('span');
                title.classList.add('eng-name');
                title.textContent = eng.name || ('引擎 ' + (i + 1));
                header.appendChild(title);
                if (eng.base) {
                    var baseEl = d.createElement('span');
                    baseEl.classList.add('eng-base');
                    baseEl.textContent = eng.base;
                    header.appendChild(baseEl);
                }

                var wrap = d.createElement('div');
                wrap.classList.add('frame-wrap');
                var frames = [0, 1].map(function (k) {
                    var f = d.createElement('iframe');
                    f.classList.add('engine-frame');
                    f.setAttribute('title', eng.name || ('引擎 ' + (i + 1)));
                    f.setAttribute('data-slot', String(k));
                    if (k === 0) f.classList.add('engine-frame-active');
                    wrap.appendChild(f);
                    return f;
                });

                var status = d.createElement('div');
                status.classList.add('frame-status');
                status.textContent = '加载中…';
                status.hidden = true;
                wrap.appendChild(status);

                panel.appendChild(header);
                panel.appendChild(wrap);
                els.resultsCol.appendChild(panel);

                var slot = { frames: frames, active: 0, loaded: [-1, -1], urls: ['', ''], status: status };
                frames.forEach(function (f, k) {
                    f.addEventListener('load', function () {
                        // 只有「正在显示」的槽位加载完成才收起加载提示
                        if (slot.active === k) slot.status.hidden = true;
                    });
                });
                slots.push(slot);
            });
        }

        function engineAt(i) { return state.engines[i] || null; }

        // 呈现第 i 个番号：能命中预取就切槽位（瞬时），否则当场加载
        function present(i) {
            var keyword = state.items[i];
            state.engines.forEach(function (eng, ei) {
                var slot = slots[ei];
                if (!slot) return;
                var url = engineUrl(eng, keyword, proto, hostname);
                var target = chooseSlot(slot, i, url);
                if (target !== slot.active) {
                    slot.frames[slot.active].classList.remove('engine-frame-active');
                    slot.frames[target].classList.add('engine-frame-active');
                    slot.active = target;
                    slot.status.hidden = true;
                    return;
                }
                if (slot.urls[target] === url && slot.loaded[target] === i) {
                    return; // 已经是这一条，不用重载
                }
                slot.loaded[target] = i;
                slot.urls[target] = url;
                slot.status.hidden = false;
                slot.frames[target].src = url;
            });
        }

        // 预取第 i 个番号到「非当前」槽位，让下一次切换瞬时完成
        function prefetch(i) {
            if (i < 0 || i >= state.items.length) return;
            var keyword = state.items[i];
            state.engines.forEach(function (eng, ei) {
                var slot = slots[ei];
                if (!slot) return;
                var other = 1 - slot.active;
                var url = engineUrl(eng, keyword, proto, hostname);
                if (slot.loaded[other] === i && slot.urls[other] === url) return;
                slot.loaded[other] = i;
                slot.urls[other] = url;
                slot.frames[other].src = url;
            });
        }

        // 强制重新加载当前番号（上游地址变了但 URL 串没变时，直接赋同样的值不会触发加载）
        function reloadCurrent() {
            if (state.index < 0 || state.index >= state.items.length) return;
            slots.forEach(function (slot) {
                slot.loaded = [-1, -1];
                slot.urls = ['', ''];
            });
            present(state.index);
        }

        function clearFrames() {
            slots.forEach(function (slot) {
                slot.loaded = [-1, -1];
                slot.urls = ['', ''];
                slot.status.hidden = true;
                slot.frames.forEach(function (f) { f.src = BLANK; });
            });
        }

        // ---------------- 番号栏 ----------------
        var nodes = [];

        function buildListNodes() {
            nodes = [];
            els.listBox.textContent = '';
            if (state.items.length === 0) {
                var empty = d.createElement('div');
                empty.classList.add('list-empty');
                empty.textContent = '还没有番号。在上方输入框里一行填一个番号，点「检索」后这里会列出全部番号，点任意一条即可看它的结果。';
                els.listBox.appendChild(empty);
                return;
            }
            state.items.forEach(function (text, i) {
                var a = d.createElement('a');
                a.href = 'javascript:void(0)';
                a.classList.add('data-line');
                a.setAttribute('data-index', String(i));
                a.setAttribute('data-text', text);
                a.textContent = text;
                a.addEventListener('click', function () { runItem(i); });
                els.listBox.appendChild(a);
                nodes.push(a);
            });
        }

        // 只刷新状态类名，不重建 DOM —— 避免列表被重建后滚动位置丢失
        function refreshListState() {
            for (var i = 0; i < nodes.length; i++) {
                var text = state.items[i];
                nodes[i].classList.toggle('current', i === state.index);
                nodes[i].classList.toggle('clicked', !!state.viewed[text]);
            }
        }

        function scrollCurrentIntoView() {
            if (state.index < 0 || !nodes[state.index]) return;
            var node = nodes[state.index];
            if (typeof node.scrollIntoView === 'function') {
                node.scrollIntoView({ block: 'nearest' });
            }
        }

        // ---------------- 翻页器 ----------------
        function renderPager() {
            var total = state.items.length;
            els.codesCount.textContent = String(total);
            if (total === 0) {
                els.pager.hidden = true;
                els.pagerInfo.textContent = '0 / 0';
                els.pagerKw.textContent = '';
                return;
            }
            els.pager.hidden = false;
            els.pagerInfo.textContent = (state.index < 0 ? 0 : state.index + 1) + ' / ' + total;
            els.pagerKw.textContent = state.index < 0 ? '（点右侧番号开始）' : state.items[state.index];
            els.prevBtn.disabled = state.index <= 0;
            els.nextBtn.disabled = state.index >= total - 1;
        }

        // ---------------- 检索 ----------------
        function markViewed(keyword) {
            if (state.viewed[keyword]) return;
            state.viewed[keyword] = true;
            var plain = {};
            for (var k in state.viewed) {
                if (Object.prototype.hasOwnProperty.call(state.viewed, k)) plain[k] = true;
            }
            writeStore('_viewed', JSON.stringify(plain));
        }

        function runItem(i) {
            if (i < 0 || i >= state.items.length) return;
            state.index = i;
            markViewed(state.items[i]);
            present(i);
            prefetch(i + 1);
            refreshListState();
            renderPager();
            scrollCurrentIntoView();
        }

        function step(direction) {
            var total = state.items.length;
            if (total === 0) return;
            var next = direction > 0 ? nextIndex(state.index, total) : prevIndex(state.index, total);
            if (next === state.index) return;
            runItem(next);
        }

        // 用一组番号替换整个列表（用户输入的多番号就落在这里）
        function setItems(list, autoRun) {
            state.items = list.slice();
            state.index = -1;
            buildListNodes();
            renderPager();
            if (autoRun && state.items.length > 0) {
                runItem(0);
            } else {
                refreshListState();
            }
        }

        function submitSearch() {
            var list = splitKeywords(els.kwInput.value);
            if (list.length === 0) {
                els.kwInput.focus();
                return;
            }
            setItems(list, true);
        }

        // ---------------- 事件 ----------------
        els.searchBtn.addEventListener('click', submitSearch);

        els.kwInput.addEventListener('keydown', function (e) {
            if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
                e.preventDefault();
                submitSearch();
            }
        });

        els.prevBtn.addEventListener('click', function () { step(-1); });
        els.nextBtn.addEventListener('click', function () { step(1); });

        d.addEventListener('keydown', function (e) {
            if (!e.altKey || state.items.length === 0) return;
            if (e.key === 'ArrowLeft') {
                e.preventDefault();
                step(-1);
            } else if (e.key === 'ArrowRight') {
                e.preventDefault();
                step(1);
            }
        });

        els.clearBtn.addEventListener('click', function () {
            state.items = [];
            state.index = -1;
            state.viewed = Object.create(null);
            writeStore('_viewed', '{}');
            buildListNodes();
            renderPager();
            clearFrames();
        });

        // ---------------- 番号栏宽度与收起 ----------------
        var codesWidth = DEFAULT_CODES_WIDTH;

        function setCodesWidth(px, persist) {
            codesWidth = clampCodesWidth(px);
            els.layout.style.setProperty('--codes-width', codesWidth + 'px');
            if (persist) writeStore('_codes_width', String(codesWidth));
        }

        function setCollapsed(collapsed, persist) {
            els.layout.classList.toggle('codes-collapsed', collapsed);
            els.codesToggle.textContent = collapsed ? '展开' : '收起';
            els.codesToggle.title = collapsed ? '展开番号栏' : '收起番号栏';
            if (persist) writeStore('_codes_collapsed', collapsed ? '1' : '0');
        }

        setCodesWidth(readStore('_codes_width', DEFAULT_CODES_WIDTH), false);
        setCollapsed(readStore('_codes_collapsed', '0') === '1', false);

        els.codesToggle.addEventListener('click', function () {
            setCollapsed(!els.layout.classList.contains('codes-collapsed'), true);
        });

        var dragging = false;
        var dragStartX = 0;
        var dragStartWidth = DEFAULT_CODES_WIDTH;

        els.splitter.addEventListener('mousedown', function (e) {
            dragging = true;
            dragStartX = e.clientX;
            dragStartWidth = codesWidth;
            els.splitter.classList.add('dragging');
            if (typeof e.preventDefault === 'function') e.preventDefault();
        });

        d.addEventListener('mousemove', function (e) {
            if (!dragging) return;
            setCodesWidth(dragStartWidth - (e.clientX - dragStartX), false);
        });

        d.addEventListener('mouseup', function () {
            if (!dragging) return;
            dragging = false;
            els.splitter.classList.remove('dragging');
            writeStore('_codes_width', String(codesWidth));
        });

        els.splitter.addEventListener('dblclick', function () {
            setCodesWidth(DEFAULT_CODES_WIDTH, true);
        });

        // ---------------- 设置：引擎增删改 ----------------
        var editorRows = [];   // [{name, base, path, css}]，与 DOM 行顺序一致

        function setHint(text, kind) {
            els.settingsHint.textContent = text || '';
            els.settingsHint.classList.remove('error');
            els.settingsHint.classList.remove('ok');
            if (kind) els.settingsHint.classList.add(kind);
        }

        function updateAddButton() {
            var full = editorRows.length >= state.maxEngines;
            els.addEngineBtn.disabled = full;
            els.addEngineBtn.textContent = full
                ? ('已达上限（' + state.maxEngines + ' 个）')
                : '+ 新增搜索引擎';
        }

        function makeField(labelText, value, cls, placeholder) {
            var wrap = d.createElement('div');
            wrap.classList.add(cls);
            var label = d.createElement('label');
            label.textContent = labelText;
            var input = d.createElement('input');
            input.setAttribute('type', 'text');
            input.setAttribute('spellcheck', 'false');
            input.value = value || '';
            if (placeholder) input.setAttribute('placeholder', placeholder);
            label.appendChild(input);
            wrap.appendChild(label);
            return { wrap: wrap, input: input };
        }

        function renderEditor(rows) {
            els.engineList.textContent = '';
            editorRows = [];

            rows.forEach(function (eng) {
                var rec = {
                    name: eng.name || '',
                    base: eng.base || '',
                    path: eng.path || '',
                    css: eng.hideCss || ''
                };

                var row = d.createElement('div');
                row.classList.add('engine-row');

                var head = d.createElement('div');
                head.classList.add('row-head');

                var fName = makeField('名称', rec.name, 'f-name', '例如 资源搜索');
                var fBase = makeField('地址', rec.base, 'f-base', 'http://192.0.2.20:8787');
                var fPath = makeField('搜索路径', rec.path, 'f-path', '/s');

                var del = d.createElement('button');
                del.classList.add('del-btn');
                del.setAttribute('type', 'button');
                del.textContent = '删除';
                del.addEventListener('click', function () {
                    syncEditor();
                    var i = editorRows.indexOf(rec);
                    if (i >= 0) editorRows.splice(i, 1);
                    renderEditor(editorRows);
                });

                head.appendChild(fName.wrap);
                head.appendChild(fBase.wrap);
                head.appendChild(fPath.wrap);
                head.appendChild(del);

                var details = d.createElement('details');
                var summary = d.createElement('summary');
                summary.textContent = '高级：隐藏该站点自身的外壳（CSS 选择器）';
                var cssLabel = d.createElement('label');
                cssLabel.textContent = '留空表示不隐藏';
                var cssInput = d.createElement('textarea');
                cssInput.setAttribute('spellcheck', 'false');
                cssInput.value = rec.css || '';
                cssLabel.appendChild(cssInput);
                details.appendChild(summary);
                details.appendChild(cssLabel);

                row.appendChild(head);
                row.appendChild(details);
                els.engineList.appendChild(row);

                rec.nodes = { name: fName.input, base: fBase.input, path: fPath.input, css: cssInput };
                editorRows.push(rec);
            });

            updateAddButton();
        }

        function syncEditor() {
            editorRows.forEach(function (rec) {
                if (!rec.nodes) return;
                rec.name = String(rec.nodes.name.value || '').trim();
                rec.base = String(rec.nodes.base.value || '').trim();
                rec.path = String(rec.nodes.path.value || '').trim();
                rec.css = String(rec.nodes.css.value || '').trim();
            });
        }

        function collectEngines() {
            syncEditor();
            return editorRows.map(function (rec) {
                var e = { name: rec.name, base: rec.base, path: rec.path };
                if (rec.css) e.hideCss = rec.css;
                return e;
            });
        }

        function openSettings() {
            // 这个网站本身就支持带关键字打开 —— 把地址显示出来，别的程序照着拼就行
            els.directLink.textContent = proto + '//' + window.location.host + '/?q=关键字';
            renderEditor(state.engines);
            setHint('当前生效来源：' + (state.origin || '未知'), '');
            if (typeof els.dialog.showModal === 'function') {
                els.dialog.showModal();
            } else if (typeof els.dialog.show === 'function') {
                els.dialog.show();
            }
        }

        function applyConfig(data) {
            if (!data || typeof data !== 'object') return;
            if (Array.isArray(data.engines)) {
                state.engines = data.engines;
                buildEnginePanels();
            }
            if (typeof data.origin === 'string') state.origin = data.origin;
            if (typeof data.maxEngines === 'number') state.maxEngines = data.maxEngines;
        }

        function saveSettings() {
            var payload = { engines: collectEngines() };
            setHint('正在保存…', '');
            els.settingsSave.disabled = true;

            window.fetch('/api/config', {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(payload)
            }).then(function (resp) {
                return resp.json().then(
                    function (data) { return { ok: resp.ok, data: data }; },
                    function () { return { ok: resp.ok, data: {} }; }
                );
            }).then(function (res) {
                els.settingsSave.disabled = false;
                if (!res.ok) {
                    setHint((res.data && res.data.error) ? res.data.error : '保存失败', 'error');
                    return;
                }
                applyConfig(res.data);
                setHint('已保存并生效：共 ' + state.engines.length + ' 个搜索引擎', 'ok');
                if (state.index >= 0) reloadCurrent();
            }).catch(function (err) {
                els.settingsSave.disabled = false;
                setHint('请求失败：' + (err && err.message ? err.message : String(err)), 'error');
            });
        }

        els.settingsBtn.addEventListener('click', openSettings);
        els.settingsSave.addEventListener('click', saveSettings);
        els.settingsCancel.addEventListener('click', function () {
            if (typeof els.dialog.close === 'function') els.dialog.close();
        });
        els.addEngineBtn.addEventListener('click', function () {
            syncEditor();
            if (editorRows.length >= state.maxEngines) return;
            editorRows.push({ name: '新搜索引擎', base: '', path: '/s', css: '' });
            renderEditor(editorRows);
        });

        // ---------------- 初始状态 ----------------
        buildEnginePanels();
        setItems(boot.items, false);
        els.kwInput.value = boot.items.join('\n');

        // 支持 /?q=关键字 直接检索（便于其它程序调用或做成浏览器书签）
        if (boot.q) {
            els.kwInput.value = boot.q;
            submitSearch();
        }

        state.slots = slots;
        return state;
    }

    return {
        splitKeywords: splitKeywords,
        engineUrl: engineUrl,
        chooseSlot: chooseSlot,
        nextIndex: nextIndex,
        prevIndex: prevIndex,
        indexOfKeyword: indexOfKeyword,
        clampCodesWidth: clampCodesWidth,
        init: init,
        SEPARATOR: SEPARATOR,
        DEFAULT_CODES_WIDTH: DEFAULT_CODES_WIDTH,
        MIN_CODES_WIDTH: MIN_CODES_WIDTH,
        MAX_CODES_WIDTH: MAX_CODES_WIDTH
    };
});
