/*
 * 双引擎检索面板的前端逻辑。
 *
 * 本文件既会被 //go:embed 内联进面板页，也可在 Node 下 require 做单元测试，
 * 所以刻意把纯函数（番号切分 / URL 构造 / 翻页索引 / 宽度夹取）与 DOM 初始化分开。
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

    // 源1 上「番号检索」的路径；源2 走面板页同源路径，由本地服务转发
    var SOURCE1_SEARCH_PATH = '/files';
    var SOURCE2_SEARCH_PATH = '/s';

    var DEFAULT_CODES_WIDTH = 216;
    var MIN_CODES_WIDTH = 150;
    var MAX_CODES_WIDTH = 640;

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
     * 生成两个 iframe 的目标地址。
     * @param {string} keyword 单个番号
     * @param {string} p1Base  源1 代理基址，如 http://127.0.0.1:18898
     * @returns {{source1: string, source2: string}}
     */
    function buildUrls(keyword, p1Base) {
        var q = encodeURIComponent(keyword);
        return {
            source1: String(p1Base) + SOURCE1_SEARCH_PATH + '?q=' + q,
            source2: SOURCE2_SEARCH_PATH + '?q=' + q
        };
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
            frame1: d.getElementById('frame1'),
            frame2: d.getElementById('frame2'),
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
            src1Input: d.getElementById('src1Input'),
            src2Input: d.getElementById('src2Input'),
            settingsHint: d.getElementById('settingsHint'),
            settingsSave: d.getElementById('settingsSave'),
            settingsCancel: d.getElementById('settingsCancel')
        };
        for (var name in els) {
            if (Object.prototype.hasOwnProperty.call(els, name) && !els[name]) return;
        }

        // ---------------- 引导数据 ----------------
        var boot = { items: [], source1: '', source2: '', origin: '', version: '', p1Port: 0 };
        var bootEl = d.getElementById('bootstrap');
        if (bootEl && bootEl.textContent) {
            try {
                var parsed = JSON.parse(bootEl.textContent);
                if (parsed && typeof parsed === 'object') {
                    if (Array.isArray(parsed.items)) boot.items = parsed.items;
                    if (typeof parsed.source1 === 'string') boot.source1 = parsed.source1;
                    if (typeof parsed.source2 === 'string') boot.source2 = parsed.source2;
                    if (typeof parsed.origin === 'string') boot.origin = parsed.origin;
                    if (typeof parsed.version === 'string') boot.version = parsed.version;
                    if (typeof parsed.p1Port === 'number') boot.p1Port = parsed.p1Port;
                }
            } catch (e) {
                /* 引导数据损坏时退化为空列表，页面仍可用 */
            }
        }

        var p1Port = boot.p1Port || parseInt(d.body.getAttribute('data-p1-port'), 10) || 0;
        var p1Base = window.location.protocol + '//' + window.location.hostname + ':' + p1Port;
        var storageKey = 't2h_' + (window.location.port || p1Port || 'default');

        var state = {
            items: [],
            index: -1,
            viewed: Object.create(null),
            sources: { source1: boot.source1, source2: boot.source2 },
            origin: boot.origin
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
        function loadCurrent(force) {
            if (state.index < 0 || state.index >= state.items.length) return;
            var urls = buildUrls(state.items[state.index], p1Base);
            if (!force) {
                els.frame1.src = urls.source1;
                els.frame2.src = urls.source2;
                return;
            }
            // 上游地址变了但 URL 串没变时，直接赋值同样字符串不会触发重新加载，
            // 先置空再回填来强制刷新。
            els.frame1.src = 'about:blank';
            els.frame2.src = 'about:blank';
            var reload = function () {
                els.frame1.src = urls.source1;
                els.frame2.src = urls.source2;
            };
            if (typeof setTimeout === 'function') {
                setTimeout(reload, 0);
            } else {
                reload();
            }
        }

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
            loadCurrent(false);
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
            els.frame1.src = 'about:blank';
            els.frame2.src = 'about:blank';
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

        // ---------------- 设置 ----------------
        function setHint(text, kind) {
            els.settingsHint.textContent = text || '';
            els.settingsHint.classList.remove('error');
            els.settingsHint.classList.remove('ok');
            if (kind) els.settingsHint.classList.add(kind);
        }

        function openSettings() {
            els.src1Input.value = state.sources.source1 || '';
            els.src2Input.value = state.sources.source2 || '';
            setHint('当前生效来源：' + (state.origin || '未知'), '');
            if (typeof els.dialog.showModal === 'function') {
                els.dialog.showModal();
            } else if (typeof els.dialog.show === 'function') {
                els.dialog.show();
            }
        }

        function applyConfig(data) {
            if (!data || typeof data !== 'object') return;
            if (typeof data.source1 === 'string') state.sources.source1 = data.source1;
            if (typeof data.source2 === 'string') state.sources.source2 = data.source2;
            if (typeof data.origin === 'string') state.origin = data.origin;
        }

        function saveSettings() {
            var payload = {
                source1: String(els.src1Input.value || '').trim(),
                source2: String(els.src2Input.value || '').trim()
            };
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
                setHint('已保存并生效：' + state.sources.source1 + ' / ' + state.sources.source2, 'ok');
                if (state.index >= 0) loadCurrent(true);
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

        // ---------------- 初始状态 ----------------
        setItems(boot.items, false);
        els.kwInput.value = boot.items.join('\n');

        return state;
    }

    return {
        splitKeywords: splitKeywords,
        buildUrls: buildUrls,
        nextIndex: nextIndex,
        prevIndex: prevIndex,
        indexOfKeyword: indexOfKeyword,
        clampCodesWidth: clampCodesWidth,
        init: init,
        SEPARATOR: SEPARATOR,
        SOURCE1_SEARCH_PATH: SOURCE1_SEARCH_PATH,
        SOURCE2_SEARCH_PATH: SOURCE2_SEARCH_PATH,
        DEFAULT_CODES_WIDTH: DEFAULT_CODES_WIDTH,
        MIN_CODES_WIDTH: MIN_CODES_WIDTH,
        MAX_CODES_WIDTH: MAX_CODES_WIDTH
    };
});
