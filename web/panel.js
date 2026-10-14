/*
 * 双引擎检索面板的前端逻辑。
 *
 * 本文件既会被 //go:embed 内联进面板页，也可在 Node 下 require 做单元测试，
 * 所以刻意把纯函数（关键字切分 / URL 构造 / 翻页索引）与 DOM 初始化分开。
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

    // 关键字分隔符：空白（含换行/制表）+ 英文/中文逗号 + 英文/中文分号 + 顿号
    var SEPARATOR = /[\s,，;；、]+/;

    // 源1 上「关键字搜索」的路径，panel.html 中列表项的 data-q1 也是这个前缀
    var SOURCE1_SEARCH_PATH = '/files';
    // 源2 走面板页同源路径，由本地服务转发到源2
    var SOURCE2_SEARCH_PATH = '/s';

    /**
     * 把用户输入切分成关键字列表：去空白、去重、保持原始顺序。
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
     * @param {string} keyword 单个关键字
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

    /** 下一个索引；已在末尾则原地不动。 */
    function nextIndex(index, total) {
        if (total <= 0) return 0;
        return index + 1 < total ? index + 1 : index;
    }

    /** 上一个索引；已在开头则原地不动。 */
    function prevIndex(index, total) {
        if (total <= 0) return 0;
        return index - 1 >= 0 ? index - 1 : index;
    }

    /** 关键字在列表中的位置；找不到返回 -1。 */
    function indexOfKeyword(keywords, keyword) {
        for (var i = 0; i < keywords.length; i++) {
            if (keywords[i] === keyword) return i;
        }
        return -1;
    }

    function init() {
        var frame1 = document.getElementById('frame1');
        var frame2 = document.getElementById('frame2');
        var kwInput = document.getElementById('kwInput');
        var searchBtn = document.getElementById('searchBtn');
        var prevBtn = document.getElementById('prevBtn');
        var nextBtn = document.getElementById('nextBtn');
        var pager = document.getElementById('pager');
        var pagerInfo = document.getElementById('pagerInfo');
        var pagerKw = document.getElementById('pagerKw');
        var clearBtn = document.getElementById('clearBtn');

        if (!frame1 || !frame2 || !kwInput || !searchBtn || !pager ||
            !prevBtn || !nextBtn || !pagerInfo || !pagerKw || !clearBtn) {
            return;
        }

        var p1Port = document.body.getAttribute('data-p1-port') || '';
        var p1Base = window.location.protocol + '//' + window.location.hostname + ':' + p1Port;

        var state = { keywords: [], index: 0 };

        // 已点击过的列表项按「来源端口」分别记录，避免不同实例互相污染
        var storageKey = 'clicked_data_items_' + window.location.port;
        var clickedItems = [];
        try {
            clickedItems = JSON.parse(localStorage.getItem(storageKey) || '[]');
            if (!Array.isArray(clickedItems)) clickedItems = [];
        } catch (e) {
            clickedItems = [];
        }

        function renderPager() {
            var total = state.keywords.length;
            if (total === 0) {
                pager.hidden = true;
                pagerInfo.textContent = '';
                pagerKw.textContent = '';
                return;
            }
            pager.hidden = false;
            pagerInfo.textContent = (state.index + 1) + ' / ' + total;
            pagerKw.textContent = state.keywords[state.index];
            prevBtn.disabled = state.index === 0;
            nextBtn.disabled = state.index === total - 1;
        }

        function loadCurrent() {
            if (state.keywords.length === 0) return;
            var urls = buildUrls(state.keywords[state.index], p1Base);
            frame1.src = urls.source1;
            frame2.src = urls.source2;
        }

        // 用一组关键字开始检索：重置到第一条并立即加载
        function search(keywords) {
            if (!keywords || keywords.length === 0) return;
            state.keywords = keywords;
            state.index = 0;
            renderPager();
            loadCurrent();
        }

        // 跳转到指定下标；越界时收敛到边界，且不重复加载同一个关键字
        function goto(index) {
            var total = state.keywords.length;
            if (total === 0) return;
            var next = index < 0 ? 0 : (index >= total ? total - 1 : index);
            if (next === state.index) return;
            state.index = next;
            renderPager();
            loadCurrent();
        }

        // direction > 0 下一个，< 0 上一个
        function step(direction) {
            var total = state.keywords.length;
            if (total === 0) return;
            goto(direction > 0 ? nextIndex(state.index, total) : prevIndex(state.index, total));
        }

        function markClicked(link, itemId) {
            link.classList.add('clicked');
            if (itemId && clickedItems.indexOf(itemId) < 0) {
                clickedItems.push(itemId);
                try {
                    localStorage.setItem(storageKey, JSON.stringify(clickedItems));
                } catch (e) {
                    /* 隐私模式等场景下写失败可忽略 */
                }
            }
        }

        searchBtn.addEventListener('click', function () {
            search(splitKeywords(kwInput.value));
        });

        kwInput.addEventListener('keydown', function (e) {
            if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
                e.preventDefault();
                search(splitKeywords(kwInput.value));
            }
        });

        prevBtn.addEventListener('click', function () { step(-1); });
        nextBtn.addEventListener('click', function () { step(1); });

        document.addEventListener('keydown', function (e) {
            if (!e.altKey || state.keywords.length === 0) return;
            if (e.key === 'ArrowLeft') {
                e.preventDefault();
                step(-1);
            } else if (e.key === 'ArrowRight') {
                e.preventDefault();
                step(1);
            }
        });

        clearBtn.addEventListener('click', function () {
            state.keywords = [];
            state.index = 0;
            renderPager();
            frame1.src = 'about:blank';
            frame2.src = 'about:blank';
        });

        var links = document.querySelectorAll('.data-line');
        Array.prototype.forEach.call(links, function (link) {
            var itemId = link.getAttribute('data-id');
            if (itemId && clickedItems.indexOf(itemId) >= 0) {
                link.classList.add('clicked');
            }
            link.addEventListener('click', function () {
                var text = (link.textContent || '').trim();
                if (text === '') return;
                kwInput.value = text;
                search([text]);
                markClicked(link, itemId);
            });
        });

        renderPager();
    }

    return {
        splitKeywords: splitKeywords,
        buildUrls: buildUrls,
        nextIndex: nextIndex,
        prevIndex: prevIndex,
        indexOfKeyword: indexOfKeyword,
        init: init,
        SEPARATOR: SEPARATOR,
        SOURCE1_SEARCH_PATH: SOURCE1_SEARCH_PATH,
        SOURCE2_SEARCH_PATH: SOURCE2_SEARCH_PATH
    };
});
