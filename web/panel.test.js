'use strict';

/*
 * panel.js 的单元测试（Node 直接运行，无第三方依赖）：
 *   node web/panel.test.js
 * 退出码非 0 表示有用例失败。
 *
 * 分两段：
 *   1. 纯函数测试（切分 / URL 构造 / 槽位选择 / 翻页索引 / 宽度夹取）
 *   2. DOM 行为测试 —— 用自建的最小 DOM stub 真正驱动一遍交互
 */

const path = require('path');
const api = require(path.join(__dirname, 'panel.js'));

let checks = 0;
let fails = 0;

function ok(name, fn) {
    checks++;
    try {
        fn();
        console.log('  [OK]   ' + name);
    } catch (err) {
        fails++;
        console.log('  [FAIL] ' + name + ' -> ' + err.message);
    }
}

async function okAsync(name, fn) {
    checks++;
    try {
        await fn();
        console.log('  [OK]   ' + name);
    } catch (err) {
        fails++;
        console.log('  [FAIL] ' + name + ' -> ' + err.message);
    }
}

function eq(actual, expected, label) {
    const a = JSON.stringify(actual);
    const e = JSON.stringify(expected);
    if (a !== e) throw new Error((label || '值不相等') + '：实际 ' + a + '，期望 ' + e);
}

function truthy(value, label) {
    if (!value) throw new Error(label || '期望为真，实际为假');
}

function falsy(value, label) {
    if (value) throw new Error(label || '期望为假，实际为真');
}

function includes(haystack, needle, label) {
    if (String(haystack).indexOf(needle) === -1) {
        throw new Error((label || '未包含期望内容') + '：' + JSON.stringify(String(haystack).slice(0, 200)));
    }
}

// ---------------------------------------------------------------------------
// 一、纯函数
// ---------------------------------------------------------------------------

console.log('== splitKeywords：切分与去重');

ok('空输入返回空数组', () => {
    eq(api.splitKeywords(''), []);
    eq(api.splitKeywords('   '), []);
    eq(api.splitKeywords('\n\t '), []);
    eq(api.splitKeywords(null), []);
    eq(api.splitKeywords(undefined), []);
});

ok('单个番号原样返回', () => {
    eq(api.splitKeywords('SSIS-001'), ['SSIS-001']);
    eq(api.splitKeywords('  ABP-123  '), ['ABP-123']);
});

ok('换行分隔（一行一个番号）', () => {
    eq(api.splitKeywords('ABP-123\nSSIS-001\nIPX-777'), ['ABP-123', 'SSIS-001', 'IPX-777']);
});

ok('Windows 换行 CRLF 也能正确切分', () => {
    eq(api.splitKeywords('ABP-123\r\nSSIS-001\r\n'), ['ABP-123', 'SSIS-001']);
});

ok('空行被忽略', () => {
    eq(api.splitKeywords('A\n\n\nB\n   \nC'), ['A', 'B', 'C']);
});

ok('逗号 / 分号 / 顿号分隔', () => {
    eq(api.splitKeywords('a,b,c'), ['a', 'b', 'c']);
    eq(api.splitKeywords('甲，乙，丙'), ['甲', '乙', '丙']);
    eq(api.splitKeywords('a;b；c、d'), ['a', 'b', 'c', 'd']);
});

ok('行内空格不拆分（一个番号就是一行）', () => {
    eq(api.splitKeywords('A B C'), ['A B C']);
    eq(api.splitKeywords('ABP-123 ABP-124\nSSIS-001'), ['ABP-123 ABP-124', 'SSIS-001']);
});

ok('重复番号去重且保持首次出现顺序', () => {
    eq(api.splitKeywords('b\na\nb\na\nc'), ['b', 'a', 'c']);
    eq(api.splitKeywords('A\na'), ['A', 'a']);
});

ok('50 条番号能一次性切分出来', () => {
    const lines = [];
    for (let i = 0; i < 50; i++) lines.push('CODE-' + i);
    eq(api.splitKeywords(lines.join('\n')).length, 50);
});

console.log('== engineUrl：按引擎生成检索地址');

const ENG = { name: '资源搜索', base: 'http://192.0.2.20:8787', path: '/s', proxyPort: 18897 };

ok('用页面协议与主机名拼装（局域网访问才正确）', () => {
    eq(api.engineUrl(ENG, 'abc', 'http:', '192.0.2.9'), 'http://192.0.2.9:18897/s?q=abc');
});

ok('番号做 URL 编码', () => {
    const u = api.engineUrl(ENG, '中 文&x', 'http:', 'h');
    includes(u, '?q=' + encodeURIComponent('中 文&x'));
    falsy(u.indexOf('?q=') !== -1 && u.slice(u.indexOf('?q=')).indexOf(' ') !== -1,
        '查询串里不应出现裸空格');
});

ok('搜索路径缺前导斜杠时自动补上', () => {
    const e = Object.assign({}, ENG, { path: 's' });
    eq(api.engineUrl(e, 'x', 'http:', 'h'), 'http://h:18897/s?q=x');
});

ok('路径为空时退化为根路径', () => {
    const e = Object.assign({}, ENG, { path: '' });
    eq(api.engineUrl(e, 'x', 'http:', 'h'), 'http://h:18897/?q=x');
});

ok('没有分配端口时返回空白页，不会拼出坏地址', () => {
    eq(api.engineUrl({ name: 'x', path: '/s', proxyPort: 0 }, 'k', 'http:', 'h'), 'about:blank');
    eq(api.engineUrl(null, 'k', 'http:', 'h'), 'about:blank');
});

console.log('== chooseSlot：预取命中就切槽位（瞬时呈现）');

ok('另一个槽位已装好同一条 → 切过去', () => {
    eq(api.chooseSlot({ active: 0, loaded: [3, 4], urls: ['u3', 'u4'] }, 4, 'u4'), 1);
});

ok('另一个槽位装的是别的条 → 仍用当前槽位', () => {
    eq(api.chooseSlot({ active: 0, loaded: [3, 4], urls: ['u3', 'u4'] }, 7, 'u7'), 0);
});

ok('下标相同但地址变了（换了引擎）→ 不能复用', () => {
    eq(api.chooseSlot({ active: 0, loaded: [3, 4], urls: ['u3', 'u4'] }, 4, 'u4-changed'), 0);
});

ok('尚未预取过 → 用当前槽位', () => {
    eq(api.chooseSlot({ active: 1, loaded: [-1, -1], urls: ['', ''] }, 0, 'u0'), 1);
});

console.log('== 翻页索引：含「尚未开始」的起点');

ok('index = -1 表示尚未开始，下一个从 0 开始', () => {
    eq(api.nextIndex(-1, 3), 0);
    eq(api.nextIndex(-1, 0), -1);
});

ok('尚未开始时上一个保持不动', () => eq(api.prevIndex(-1, 3), -1));

ok('nextIndex 在末尾保持不动', () => {
    eq(api.nextIndex(0, 3), 1);
    eq(api.nextIndex(2, 3), 2);
});

ok('prevIndex 在开头保持不动', () => {
    eq(api.prevIndex(2, 3), 1);
    eq(api.prevIndex(0, 3), 0);
});

ok('50 条番号完整走一趟翻页不越界', () => {
    let i = -1;
    for (let k = 0; k < 60; k++) i = api.nextIndex(i, 50);
    eq(i, 49);
    for (let k = 0; k < 60; k++) i = api.prevIndex(i, 50);
    eq(i, 0);
});

ok('indexOfKeyword 命中与未命中', () => {
    eq(api.indexOfKeyword(['a', 'b'], 'b'), 1);
    eq(api.indexOfKeyword(['a', 'b'], 'z'), -1);
    eq(api.indexOfKeyword([], 'a'), -1);
});

console.log('== 番号栏宽度夹取');

ok('默认值与上下限', () => {
    eq(api.clampCodesWidth(undefined), api.DEFAULT_CODES_WIDTH);
    eq(api.clampCodesWidth('abc'), api.DEFAULT_CODES_WIDTH);
    eq(api.clampCodesWidth(10), api.MIN_CODES_WIDTH);
    eq(api.clampCodesWidth(9999), api.MAX_CODES_WIDTH);
    eq(api.clampCodesWidth(300), 300);
});

// ---------------------------------------------------------------------------
// 二、DOM 行为
// ---------------------------------------------------------------------------

function makeStyle() {
    const props = {};
    return {
        setProperty(k, v) { props[k] = String(v); },
        getPropertyValue(k) { return k in props ? props[k] : ''; },
        _props: props
    };
}

function makeEl(id, tag) {
    const el = {
        id: id || '',
        tagName: (tag || 'div').toUpperCase(),
        children: [],
        _text: undefined,
        attrs: Object.create(null),
        listeners: Object.create(null),
        classes: Object.create(null),
        src: '',
        hidden: false,
        disabled: false,
        value: '',
        title: '',
        href: '',
        style: makeStyle(),
        parentNode: null
    };

    Object.defineProperty(el, 'textContent', {
        get() {
            if (el._text !== undefined) return el._text;
            return el.children.map((c) => c.textContent).join('');
        },
        set(v) {
            el._text = String(v);
            if (el._text === '') el.children.length = 0;
        },
        configurable: true
    });

    el.classList = {
        add(...cs) { cs.forEach((c) => { el.classes[c] = true; }); },
        remove(...cs) { cs.forEach((c) => { delete el.classes[c]; }); },
        contains(c) { return !!el.classes[c]; },
        toggle(c, force) {
            const on = force === undefined ? !el.classes[c] : !!force;
            if (on) el.classes[c] = true; else delete el.classes[c];
            return on;
        }
    };
    el.appendChild = (child) => { child.parentNode = el; el.children.push(child); return child; };
    el.setAttribute = (k, v) => { el.attrs[k] = String(v); };
    el.getAttribute = (k) => (k in el.attrs ? el.attrs[k] : null);
    el.addEventListener = (t, fn) => { (el.listeners[t] = el.listeners[t] || []).push(fn); };
    el.fire = (t, ev) => { (el.listeners[t] || []).slice().forEach((fn) => fn(ev || { preventDefault() {} })); };
    el.querySelectorAll = (sel) => {
        const want = String(sel).replace(/^\./, '');
        return el.children.filter((c) => c.classes[want]);
    };
    // 递归查找带指定 class 的后代，便于断言「面板里长出了几个引擎」
    el.findAll = (cls) => {
        const out = [];
        (function walk(n) {
            n.children.forEach((c) => {
                if (c.classes[cls]) out.push(c);
                walk(c);
            });
        })(el);
        return out;
    };
    el.focus = () => {};
    el.scrollIntoView = () => {};
    return el;
}

const HIDE_PRESETS = [
    { key: 'none', label: '不隐藏', css: '' },
    { key: 'common', label: '通用（顶栏 / 侧边栏 / 页脚）', css: 'header,aside,footer{display:none!important}' },
    { key: '115', label: '115 Lite Web', css: 'header,aside,footer,[data-sidebar="sidebar"]{display:none!important}' },
    { key: 'search', label: '资源搜索', css: 'header.topbar,#filter-bar{display:none!important}' }
];

const ENGINES_BOOT = [
    { name: '115 Lite Web', base: 'http://192.0.2.20:11510', path: '/files', proxyPort: 18898 },
    { name: '资源搜索', base: 'http://192.0.2.20:8787', path: '/s', proxyPort: 18897 }
];
const BOOT_ITEMS = ['ABP-123', 'SSIS-001', 'IPX-777'];
const ALL_IDS = ['resultsCol', 'kwInput', 'searchBtn', 'settingsBtn', 'prevBtn', 'nextBtn',
    'pager', 'pagerInfo', 'pagerKw', 'clearBtn', 'listBox', 'codesCount',
    'codesToggle', 'layout', 'splitter', 'settingsDialog', 'directLink', 'engineList',
    'addEngineBtn', 'settingsHint', 'settingsSave', 'settingsCancel', 'bootstrap'];

function buildDoc(boot, ids) {
    ids.pager.hidden = true;
    ids.settingsDialog.showModal = function () { this._open = true; };
    ids.settingsDialog.close = function () { this._open = false; };
    ids.bootstrap.textContent = JSON.stringify(boot);
}

const byId = {};
ALL_IDS.forEach((id) => { byId[id] = makeEl(id, id === 'kwInput' ? 'textarea' : 'div'); });
buildDoc({
    items: BOOT_ITEMS, engines: ENGINES_BOOT, hidePresets: HIDE_PRESETS, origin: '内置默认值',
    version: 'test', q: '', maxEngines: 8
}, byId);

const docListeners = Object.create(null);
const localStore = Object.create(null);

global.document = {
    readyState: 'complete',
    body: makeEl('body', 'body'),
    getElementById: (id) => byId[id] || null,
    createElement: (tag) => makeEl('', tag),
    addEventListener(t, fn) { (docListeners[t] = docListeners[t] || []).push(fn); },
    fire(t, ev) { (docListeners[t] || []).slice().forEach((fn) => fn(ev || { preventDefault() {} })); }
};

global.window = {
    location: { protocol: 'http:', hostname: '127.0.0.1', port: '18899', host: '127.0.0.1:18899' },
    fetch: () => Promise.reject(new Error('测试未提供 fetch 实现'))
};
global.localStorage = {
    getItem: (k) => (k in localStore ? localStore[k] : null),
    setItem: (k, v) => { localStore[k] = String(v); },
    removeItem: (k) => { delete localStore[k]; }
};

const fetched = [];
global.window.fetch = (url, opts) => {
    fetched.push({ url, opts });
    return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({
            engines: [
                { name: '引擎A', base: 'http://192.0.2.41:1', path: '/s', proxyPort: 18898 },
                { name: '引擎B', base: 'http://192.0.2.42:2', path: '/s', proxyPort: 18897 },
                { name: '引擎C', base: 'http://192.0.2.43:3', path: '/s', proxyPort: 18896 }
            ],
            origin: '配置文件（页面保存）',
            maxEngines: 8
        })
    });
};

const state = api.init();

const STORE = 't2h_18899';
const lines = () => byId.listBox.querySelectorAll('.data-line');
const flush = async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => setTimeout(resolve, 0));
};

console.log('== DOM 行为：多引擎面板渲染');

ok('按引导数据里的引擎数量生成结果面板', () => {
    eq(byId.resultsCol.findAll('panel').length, 2);
});

ok('面板标题用引擎的名字（不再是「搜索结果1」）', () => {
    const names = byId.resultsCol.findAll('eng-name').map((n) => n.textContent);
    eq(names, ['115 Lite Web', '资源搜索']);
});

ok('每个面板有两个 iframe 槽位，默认 0 号可见', () => {
    eq(state.slots.length, 2);
    state.slots.forEach((s) => {
        eq(s.frames.length, 2);
        eq(s.frames[0].classList.contains('engine-frame-active'), true);
        eq(s.frames[1].classList.contains('engine-frame-active'), false);
    });
});

ok('引导数据里的番号被渲染进番号栏', () => {
    eq(lines().length, 3);
    eq(byId.codesCount.textContent, '3');
});

ok('初始尚未检索：翻页器 0 / 3，各引擎 iframe 都没加载', () => {
    eq(byId.pager.hidden, false);
    eq(byId.pagerInfo.textContent, '0 / 3');
    state.slots.forEach((s) => {
        eq(s.frames[0].src, '');
        eq(s.frames[1].src, '');
    });
});

console.log('== DOM 行为：点番号出结果 + 预取下一条');

ok('点第二个番号，两个引擎同时跳转', () => {
    lines()[1].fire('click');
    eq(state.slots[0].frames[0].src, 'http://127.0.0.1:18898/files?q=SSIS-001');
    eq(state.slots[1].frames[0].src, 'http://127.0.0.1:18897/s?q=SSIS-001');
    eq(byId.pagerInfo.textContent, '2 / 3');
    eq(lines()[1].classList.contains('current'), true);
});

ok('同时把下一条预取到了另一个槽位', () => {
    eq(state.slots[0].frames[1].src, 'http://127.0.0.1:18898/files?q=IPX-777');
    eq(state.slots[1].frames[1].src, 'http://127.0.0.1:18897/s?q=IPX-777');
    eq(state.slots[0].loaded[1], 2);
});

ok('点「下一个」时命中预取：切槽位而不是重新加载（瞬时呈现）', () => {
    const before = state.slots[0].frames[0].src;   // 上一条的地址，切换后应保持不动
    byId.nextBtn.fire('click');
    eq(state.slots[0].active, 1, '应切到 1 号槽位');
    eq(state.slots[0].frames[1].classList.contains('engine-frame-active'), true);
    eq(state.slots[0].frames[0].classList.contains('engine-frame-active'), false);
    eq(state.slots[0].frames[0].src, before, '0 号槽位不应被重新赋值');
    eq(state.slots[0].status.hidden, true, '命中预取时不显示加载提示');
    eq(byId.pagerInfo.textContent, '3 / 3');
});

ok('跳到未预取的番号时，改用当前槽位加载并显示加载提示', () => {
    lines()[0].fire('click');
    eq(state.slots[0].active, 1);
    eq(state.slots[0].frames[1].src, 'http://127.0.0.1:18898/files?q=ABP-123');
    eq(state.slots[0].status.hidden, false, '未命中预取应显示加载提示');
});

ok('iframe 加载完成后收起加载提示', () => {
    const slot = state.slots[0];
    slot.frames[slot.active].fire('load', {});
    eq(slot.status.hidden, true);
});

ok('看过的番号被标记并写入 localStorage', () => {
    const saved = JSON.parse(localStore[STORE + '_viewed'] || '{}');
    truthy(saved['ABP-123'] === true);
    truthy(saved['SSIS-001'] === true);
});

console.log('== DOM 行为：翻页与快捷键');

ok('已到末尾时「下一个」禁用', () => {
    lines()[2].fire('click');
    eq(byId.nextBtn.disabled, true);
    byId.nextBtn.fire('click');
    eq(byId.pagerInfo.textContent, '3 / 3');
});

ok('Alt+← / Alt+→ 快捷键翻页', () => {
    global.document.fire('keydown', { altKey: true, key: 'ArrowLeft', preventDefault() {} });
    eq(byId.pagerInfo.textContent, '2 / 3');
    global.document.fire('keydown', { altKey: true, key: 'ArrowRight', preventDefault() {} });
    eq(byId.pagerInfo.textContent, '3 / 3');
});

console.log('== DOM 行为：多番号输入');

ok('输入 50 个番号后番号栏是 50 条', () => {
    const list = [];
    for (let i = 0; i < 50; i++) list.push('CODE-' + i);
    byId.kwInput.value = list.join('\n');
    byId.searchBtn.fire('click');
    eq(byId.codesCount.textContent, '50');
    eq(lines().length, 50);
});

ok('检索后自动展示第一条，previous 禁用', () => {
    eq(byId.pagerInfo.textContent, '1 / 50');
    eq(byId.prevBtn.disabled, true);
    const slot = state.slots[0];
    eq(slot.frames[slot.active].src, 'http://127.0.0.1:18898/files?q=CODE-0',
        '第一条应加载到当前可见的槽位上');
});

ok('翻到第 50 条不越界', () => {
    for (let i = 0; i < 60; i++) byId.nextBtn.fire('click');
    eq(byId.pagerInfo.textContent, '50 / 50');
    eq(byId.nextBtn.disabled, true);
});

ok('番号栏宽度与收起状态仍可用（列表自身滚动由 CSS 保证）', () => {
    eq(byId.layout.style.getPropertyValue('--codes-width'), api.DEFAULT_CODES_WIDTH + 'px');
    byId.codesToggle.fire('click');
    eq(byId.layout.classList.contains('codes-collapsed'), true);
    byId.codesToggle.fire('click');
    eq(byId.layout.classList.contains('codes-collapsed'), false);
});

ok('清空会复位列表与所有引擎的 iframe', () => {
    byId.clearBtn.fire('click');
    eq(byId.codesCount.textContent, '0');
    eq(byId.pager.hidden, true);
    state.slots.forEach((s) => {
        s.frames.forEach((f) => eq(f.src, 'about:blank'));
    });
});

// ---------------------------------------------------------------------------
// 三、设置（需要等异步）
// ---------------------------------------------------------------------------

(async function () {
    console.log('== DOM 行为：搜索引擎的增删改');

    await okAsync('打开设置会按当前引擎生成编辑行', async () => {
        byId.settingsBtn.fire('click');
        eq(byId.engineList.findAll('engine-row').length, 2);
        includes(byId.settingsHint.textContent, '内置默认值');
    });

    await okAsync('设置里显示「这个网站带关键字」的直达地址', async () => {
        eq(byId.directLink.textContent, 'http://127.0.0.1:18899/?q=关键字');
    });

    await okAsync('「隐藏网站外壳」提供现成预设，点一下就填进去（不用自己写选择器）', async () => {
        const row = byId.engineList.findAll('engine-row')[0];
        const labels = row.findAll('preset-btn').map((b) => b.textContent);
        eq(labels, ['不隐藏', '通用（顶栏 / 侧边栏 / 页脚）', '115 Lite Web', '资源搜索']);
        const css = row.findAll('css-input')[0];
        eq(css.value, '', '初始为空表示不隐藏');
    });

    await okAsync('点「通用」预设会把对应 CSS 填进输入框', async () => {
        const row = byId.engineList.findAll('engine-row')[0];
        const btns = row.findAll('preset-btn');
        const css = row.findAll('css-input')[0];
        btns[1].fire('click');
        eq(css.value, HIDE_PRESETS[1].css);
        btns[2].fire('click');
        eq(css.value, HIDE_PRESETS[2].css);
        btns[0].fire('click');
        eq(css.value, '');
    });

    await okAsync('新建的引擎默认带「通用」预设，不用面对空白框', async () => {
        byId.addEngineBtn.fire('click');
        const rows = byId.engineList.findAll('engine-row');
        eq(rows.length, 3);
        const css = rows[2].findAll('css-input')[0];
        eq(css.value, HIDE_PRESETS[1].css, '新建引擎应默认带上通用预设');
    });

    await okAsync('点「新增搜索引擎」多出一行（上一用例已新增一次，这里再点一次）', async () => {
        const before = byId.engineList.findAll('engine-row').length;
        byId.addEngineBtn.fire('click');
        eq(byId.engineList.findAll('engine-row').length, before + 1);
    });

    await okAsync('「删除」会移除对应那一行', async () => {
        const rows = byId.engineList.findAll('engine-row');
        const before = rows.length;
        rows[1].findAll('del-btn')[0].fire('click');
        eq(byId.engineList.findAll('engine-row').length, before - 1);
    });

    await okAsync('保存时把当前编辑内容 POST 给 /api/config', async () => {
        fetched.length = 0;
        const expectCount = byId.engineList.findAll('engine-row').length;
        byId.settingsSave.fire('click');
        eq(fetched.length, 1);
        eq(fetched[0].url, '/api/config');
        const body = JSON.parse(fetched[0].opts.body);
        eq(body.engines.length, expectCount);
        eq(body.engines[0].name, '115 Lite Web');
        eq(body.engines[0].base, 'http://192.0.2.20:11510');
        eq(body.engines[0].path, '/files');
        await flush();
    });

    await okAsync('保存成功后按新引擎重建结果面板', async () => {
        await flush();
        includes(byId.settingsHint.textContent, '已保存并生效');
        truthy(byId.settingsHint.classList.contains('ok'), '应显示成功样式');
        const names = byId.resultsCol.findAll('eng-name').map((n) => n.textContent);
        eq(names, ['引擎A', '引擎B', '引擎C']);
        eq(state.slots.length, 3);
    });

    await okAsync('保存失败时把后端返回的错误原样显示出来', async () => {
        global.window.fetch = () => Promise.resolve({
            ok: false,
            json: () => Promise.resolve({ error: '第 2 个引擎：地址必须以 http:// 或 https:// 开头' })
        });
        byId.settingsSave.fire('click');
        await flush();
        includes(byId.settingsHint.textContent, '第 2 个引擎');
        truthy(byId.settingsHint.classList.contains('error'), '应显示错误样式');
        eq(byId.settingsSave.disabled, false);
    });

    await okAsync('网络异常时给出可读提示', async () => {
        global.window.fetch = () => Promise.reject(new Error('network down'));
        byId.settingsSave.fire('click');
        await flush();
        includes(byId.settingsHint.textContent, 'network down');
    });

    // ---------------------------------------------------------------------
    // 四、URL 直达检索（/?q=关键字）
    // ---------------------------------------------------------------------

    console.log('== DOM 行为：URL 直接带关键字');

    await okAsync('/?q=关键字 打开即自动检索', async () => {
        Object.keys(localStore).forEach((k) => delete localStore[k]);
        const freshIds = {};
        ALL_IDS.forEach((id) => { freshIds[id] = makeEl(id); });
        buildDoc({
            items: [], engines: ENGINES_BOOT, hidePresets: HIDE_PRESETS,
            origin: '配置文件（页面保存）',
            version: 'test', q: 'ABP-999', maxEngines: 8
        }, freshIds);

        const savedGet = global.document.getElementById;
        global.document.getElementById = (id) => freshIds[id] || null;
        global.window.fetch = () => Promise.resolve({ ok: true, json: () => Promise.resolve({}) });

        const freshState = api.init();
        global.document.getElementById = savedGet;

        eq(freshIds.kwInput.value, 'ABP-999');
        eq(freshIds.codesCount.textContent, '1');
        eq(freshIds.pagerInfo.textContent, '1 / 1');
        eq(freshState.slots[0].frames[0].src, 'http://127.0.0.1:18898/files?q=ABP-999');
        eq(freshState.slots[1].frames[0].src, 'http://127.0.0.1:18897/s?q=ABP-999');
    });

    console.log('\n共 ' + checks + ' 项断言，失败 ' + fails + ' 项');
    if (fails > 0) {
        process.exit(1);
    }
})();
