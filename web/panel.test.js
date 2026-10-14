'use strict';

/*
 * panel.js 的单元测试（Node 直接运行，无第三方依赖）：
 *   node web/panel.test.js
 * 退出码非 0 表示有用例失败。
 *
 * 分两段：
 *   1. 纯函数测试（切分 / URL 构造 / 翻页索引 / 宽度夹取）
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

function includes(haystack, needle, label) {
    if (String(haystack).indexOf(needle) === -1) {
        throw new Error((label || '未包含期望内容') + '：' + JSON.stringify(String(haystack).slice(0, 160)));
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

ok('英文逗号分隔', () => {
    eq(api.splitKeywords('a,b,c'), ['a', 'b', 'c']);
});

ok('中文逗号分隔', () => {
    eq(api.splitKeywords('甲，乙，丙'), ['甲', '乙', '丙']);
});

ok('分号与顿号分隔', () => {
    eq(api.splitKeywords('a;b；c、d'), ['a', 'b', 'c', 'd']);
});

ok('行内空格不拆分（一个番号就是一行）', () => {
    eq(api.splitKeywords('A B C'), ['A B C']);
    eq(api.splitKeywords('ABP-123 ABP-124\nSSIS-001'), ['ABP-123 ABP-124', 'SSIS-001']);
});

ok('行首行尾空白被裁剪', () => {
    eq(api.splitKeywords('   ABP-123   \n\tSSIS-001\t\n'), ['ABP-123', 'SSIS-001']);
});

ok('混合分隔符', () => {
    eq(api.splitKeywords(' a, b；c\n d 、\n'), ['a', 'b', 'c', 'd']);
});

ok('重复番号去重且保持首次出现顺序', () => {
    eq(api.splitKeywords('b\na\nb\na\nc'), ['b', 'a', 'c']);
    eq(api.splitKeywords('A a'), ['A a'], '空格不是分隔符，整体算一个番号');
    eq(api.splitKeywords('A\na'), ['A', 'a'], '换行分隔时才区分大小写');
});

console.log('== buildUrls：两个源的地址构造');

ok('中文番号做 URL 编码', () => {
    const u = api.buildUrls('中文', 'http://127.0.0.1:18898');
    eq(u.source1, 'http://127.0.0.1:18898/files?q=' + encodeURIComponent('中文'));
    eq(u.source2, '/s?q=' + encodeURIComponent('中文'));
});

ok('特殊字符被编码，不会破坏查询串', () => {
    const u = api.buildUrls('a&b=c d', 'http://h:1');
    if (u.source2.indexOf('&') !== -1) throw new Error('源2 地址未编码 &：' + u.source2);
    if (u.source2.indexOf(' ') !== -1) throw new Error('源2 地址未编码空格：' + u.source2);
});

ok('源2 始终是同源相对路径', () => {
    const u = api.buildUrls('x', 'http://127.0.0.1:1');
    truthy(u.source2.indexOf('/s?q=') === 0, '源2 不是相对路径：' + u.source2);
});

console.log('== 翻页索引：含「尚未开始」的起点');

ok('index = -1 表示尚未开始，下一个从 0 开始', () => {
    eq(api.nextIndex(-1, 3), 0);
    eq(api.nextIndex(-1, 0), -1);
});

ok('尚未开始时上一个保持不动', () => {
    eq(api.prevIndex(-1, 3), -1);
});

ok('nextIndex 在末尾保持不动', () => {
    eq(api.nextIndex(0, 3), 1);
    eq(api.nextIndex(2, 3), 2);
    eq(api.nextIndex(0, 1), 0);
});

ok('prevIndex 在开头保持不动', () => {
    eq(api.prevIndex(2, 3), 1);
    eq(api.prevIndex(0, 3), 0);
});

ok('多番号完整翻页序列走一遍', () => {
    const kws = api.splitKeywords('A\nB\nC');
    eq(kws, ['A', 'B', 'C']);
    let i = -1;
    i = api.nextIndex(i, kws.length);
    eq(kws[i], 'A');
    i = api.nextIndex(i, kws.length);
    eq(kws[i], 'B');
    i = api.nextIndex(i, kws.length);
    eq(kws[i], 'C');
    i = api.nextIndex(i, kws.length);
    eq(kws[i], 'C', '末尾不应越界');
    i = api.prevIndex(i, kws.length);
    eq(kws[i], 'B');
    i = api.prevIndex(i, kws.length);
    eq(kws[i], 'A');
    i = api.prevIndex(i, kws.length);
    eq(kws[i], 'A', '开头不应越界');
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

ok('默认宽度明显小于改版前的 320px', () => {
    truthy(api.DEFAULT_CODES_WIDTH < 320, '默认宽度应比 320 更窄');
});

console.log('== 契约：与后端注入的占位符保持一致');

ok('源1 搜索路径为 /files', () => eq(api.SOURCE1_SEARCH_PATH, '/files'));
ok('源2 搜索路径为 /s', () => eq(api.SOURCE2_SEARCH_PATH, '/s'));

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
    el.focus = () => {};
    el.scrollIntoView = () => {};
    return el;
}

const P1_PORT = 18898;
const P1_BASE = 'http://127.0.0.1:' + P1_PORT;
const BOOT_ITEMS = ['ABP-123', 'SSIS-001', 'IPX-777'];

const byId = {};
['frame1', 'frame2', 'kwInput', 'searchBtn', 'settingsBtn', 'prevBtn', 'nextBtn',
    'pager', 'pagerInfo', 'pagerKw', 'clearBtn', 'listBox', 'codesCount',
    'codesToggle', 'layout', 'splitter', 'settingsDialog', 'src1Input', 'src2Input',
    'settingsHint', 'settingsSave', 'settingsCancel', 'bootstrap'].forEach((id) => {
    const el = makeEl(id, id.indexOf('frame') === 0 ? 'iframe' : (id === 'kwInput' ? 'textarea' : 'div'));
    byId[id] = el;
});

byId.pager.hidden = true;
byId.settingsDialog.showModal = function () { this._open = true; };
byId.settingsDialog.close = function () { this._open = false; };
byId.bootstrap.textContent = JSON.stringify({
    items: BOOT_ITEMS,
    source1: 'http://192.0.2.20:11510',
    source2: 'http://192.0.2.20:8787',
    origin: '内置默认值',
    version: 'test',
    p1Port: P1_PORT
});

const docListeners = Object.create(null);
const localStore = Object.create(null);

global.document = {
    readyState: 'complete',
    body: Object.assign(makeEl('body', 'body'), {}),
    getElementById: (id) => byId[id] || null,
    createElement: (tag) => makeEl('', tag),
    addEventListener(t, fn) { (docListeners[t] = docListeners[t] || []).push(fn); },
    fire(t, ev) { (docListeners[t] || []).slice().forEach((fn) => fn(ev || { preventDefault() {} })); }
};
global.document.body.setAttribute('data-p1-port', String(P1_PORT));

global.window = {
    location: { protocol: 'http:', hostname: '127.0.0.1', port: '18899' },
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
            source1: 'http://192.0.2.31:1',
            source2: 'http://192.0.2.32:2',
            origin: '配置文件（页面保存）'
        })
    });
};

api.init();

const STORE = 't2h_18899';
const lines = () => byId.listBox.querySelectorAll('.data-line');
// 两轮宏任务：保存链路是 fetch -> .then -> .then -> setTimeout(强制刷新)，
// 一轮只够把微任务排空，强制刷新的定时器要到第二轮才跑。
const flush = async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
    await new Promise((resolve) => setTimeout(resolve, 0));
};

console.log('== DOM 行为：初始化');

ok('引导数据里的番号被渲染进番号栏', () => {
    eq(lines().length, 3);
    eq(lines().map((n) => n.textContent), BOOT_ITEMS);
    eq(byId.codesCount.textContent, '3');
});

ok('初始尚未检索：翻页器显示 0 / 3，两个 iframe 空白', () => {
    eq(byId.pager.hidden, false);
    eq(byId.pagerInfo.textContent, '0 / 3');
    eq(byId.prevBtn.disabled, true);
    eq(byId.nextBtn.disabled, false);
    eq(byId.frame1.src, '');
    eq(byId.frame2.src, '');
});

ok('输入框被预填为 txt 里的番号（一行一个）', () => {
    eq(byId.kwInput.value, BOOT_ITEMS.join('\n'));
});

console.log('== DOM 行为：点击番号栏出结果');

ok('点第二个番号，两个 iframe 同时跳转且该项高亮', () => {
    lines()[1].fire('click');
    eq(byId.frame1.src, P1_BASE + '/files?q=SSIS-001');
    eq(byId.frame2.src, '/s?q=SSIS-001');
    eq(byId.pagerInfo.textContent, '2 / 3');
    eq(byId.pagerKw.textContent, 'SSIS-001');
    eq(lines()[1].classList.contains('current'), true);
    eq(lines()[0].classList.contains('current'), false);
});

ok('看过的番号被标记并写入 localStorage', () => {
    eq(lines()[1].classList.contains('clicked'), true);
    const saved = JSON.parse(localStore[STORE + '_viewed'] || '{}');
    eq(saved, { 'SSIS-001': true });
});

console.log('== DOM 行为：翻页');

ok('点「下一个」切到下一条', () => {
    byId.nextBtn.fire('click');
    eq(byId.pagerInfo.textContent, '3 / 3');
    eq(byId.frame1.src, P1_BASE + '/files?q=IPX-777');
    eq(byId.nextBtn.disabled, true);
});

ok('到末尾后「下一个」不再越界', () => {
    byId.nextBtn.fire('click');
    eq(byId.pagerInfo.textContent, '3 / 3');
});

ok('点「上一个」回退', () => {
    byId.prevBtn.fire('click');
    eq(byId.pagerInfo.textContent, '2 / 3');
    eq(byId.pagerKw.textContent, 'SSIS-001');
});

ok('Alt+← 快捷键翻页', () => {
    global.document.fire('keydown', { altKey: true, key: 'ArrowLeft', preventDefault() {} });
    eq(byId.pagerInfo.textContent, '1 / 3');
    eq(byId.frame1.src, P1_BASE + '/files?q=ABP-123');
});

ok('Alt+→ 快捷键翻页', () => {
    global.document.fire('keydown', { altKey: true, key: 'ArrowRight', preventDefault() {} });
    eq(byId.pagerInfo.textContent, '2 / 3');
});

console.log('== DOM 行为：输入多个番号');

ok('输入框里一行一个番号，点检索后番号栏被整体替换', () => {
    byId.kwInput.value = 'AAA-001\nBBB-002\nCCC-003\nDDD-004';
    byId.searchBtn.fire('click');
    eq(byId.codesCount.textContent, '4');
    eq(lines().map((n) => n.textContent), ['AAA-001', 'BBB-002', 'CCC-003', 'DDD-004']);
});

ok('检索后自动展示第一条，previous 禁用', () => {
    eq(byId.pagerInfo.textContent, '1 / 4');
    eq(byId.frame1.src, P1_BASE + '/files?q=AAA-001');
    eq(byId.prevBtn.disabled, true);
    eq(byId.nextBtn.disabled, false);
});

ok('Ctrl+Enter 也能触发检索', () => {
    byId.kwInput.value = 'X-1, X-2';
    byId.kwInput.fire('keydown', { key: 'Enter', ctrlKey: true, preventDefault() {} });
    eq(byId.codesCount.textContent, '2');
    eq(lines().map((n) => n.textContent), ['X-1', 'X-2']);
});

ok('空白输入不会清掉现有列表', () => {
    byId.kwInput.value = '   \n  ';
    byId.searchBtn.fire('click');
    eq(byId.codesCount.textContent, '2');
});

console.log('== DOM 行为：番号栏宽度与收起');

ok('默认宽度已应用且比旧的 320px 窄', () => {
    const w = byId.layout.style.getPropertyValue('--codes-width');
    eq(w, api.DEFAULT_CODES_WIDTH + 'px');
    truthy(api.DEFAULT_CODES_WIDTH < 320, '默认应更窄');
});

ok('拖动分隔条可调整宽度并记录到 localStorage', () => {
    byId.splitter.fire('mousedown', { clientX: 800, preventDefault() {} });
    global.document.fire('mousemove', { clientX: 700 });
    global.document.fire('mouseup', {});
    eq(byId.layout.style.getPropertyValue('--codes-width'), '316px');
    eq(localStore[STORE + '_codes_width'], '316');
});

ok('拖动超过下限时被夹住', () => {
    byId.splitter.fire('mousedown', { clientX: 800, preventDefault() {} });
    global.document.fire('mousemove', { clientX: 5000 });
    global.document.fire('mouseup', {});
    eq(byId.layout.style.getPropertyValue('--codes-width'), api.MIN_CODES_WIDTH + 'px');
});

ok('双击分隔条复位到默认宽度', () => {
    byId.splitter.fire('dblclick', {});
    eq(byId.layout.style.getPropertyValue('--codes-width'), api.DEFAULT_CODES_WIDTH + 'px');
});

ok('收起 / 展开番号栏', () => {
    eq(byId.codesToggle.textContent, '收起');
    byId.codesToggle.fire('click');
    eq(byId.layout.classList.contains('codes-collapsed'), true);
    eq(byId.codesToggle.textContent, '展开');
    eq(localStore[STORE + '_codes_collapsed'], '1');
    byId.codesToggle.fire('click');
    eq(byId.layout.classList.contains('codes-collapsed'), false);
});

console.log('== DOM 行为：清空');

ok('清空会复位列表、iframe 与翻页器', () => {
    byId.clearBtn.fire('click');
    eq(byId.codesCount.textContent, '0');
    eq(byId.pager.hidden, true);
    eq(byId.frame1.src, 'about:blank');
    eq(byId.frame2.src, 'about:blank');
    eq(lines().length, 0);
});

ok('清空后列表里给出占位提示', () => {
    const empty = byId.listBox.querySelectorAll('.list-empty');
    eq(empty.length, 1);
});

ok('清空后快捷键与翻页按钮不报错', () => {
    global.document.fire('keydown', { altKey: true, key: 'ArrowRight', preventDefault() {} });
    byId.nextBtn.fire('click');
    byId.prevBtn.fire('click');
    eq(byId.codesCount.textContent, '0');
});

// ---------------------------------------------------------------------------
// 三、设置（需要等异步）
// ---------------------------------------------------------------------------

(async function () {
    console.log('== DOM 行为：上游地址设置');

    await okAsync('打开设置会用当前地址回填输入框', async () => {
        byId.settingsBtn.fire('click');
        eq(byId.src1Input.value, 'http://192.0.2.20:11510');
        eq(byId.src2Input.value, 'http://192.0.2.20:8787');
        includes(byId.settingsHint.textContent, '内置默认值');
    });

    await okAsync('保存会把两个地址 POST 给 /api/config', async () => {
        fetched.length = 0;
        byId.src1Input.value = '  http://192.0.2.31:1  ';
        byId.src2Input.value = 'http://192.0.2.32:2';
        byId.settingsSave.fire('click');
        eq(fetched.length, 1);
        eq(fetched[0].url, '/api/config');
        eq(fetched[0].opts.method, 'POST');
        eq(JSON.parse(fetched[0].opts.body), {
            source1: 'http://192.0.2.31:1',
            source2: 'http://192.0.2.32:2'
        });
        await flush();
    });

    await okAsync('保存成功后提示生效并更新本地状态', async () => {
        await flush();
        includes(byId.settingsHint.textContent, '已保存并生效');
        truthy(byId.settingsHint.classList.contains('ok'), '应显示成功样式');
        eq(byId.settingsSave.disabled, false);
    });

    await okAsync('保存成功后重新加载当前番号（地址已换，需要强制刷新）', async () => {
        // 先重新检索一条，制造「当前有结果」的状态
        byId.kwInput.value = 'ZZZ-001';
        byId.searchBtn.fire('click');
        eq(byId.frame1.src, P1_BASE + '/files?q=ZZZ-001');

        byId.settingsSave.fire('click');
        await flush();
        eq(byId.frame1.src, P1_BASE + '/files?q=ZZZ-001', '强制刷新后应回到同一个地址');

        byId.settingsBtn.fire('click');
        eq(byId.src1Input.value, 'http://192.0.2.31:1', '再次打开设置应显示新地址');
    });

    await okAsync('保存失败时把后端返回的错误原样显示出来', async () => {
        global.window.fetch = () => Promise.resolve({
            ok: false,
            json: () => Promise.resolve({ error: '源1 地址无效：地址必须以 http:// 或 https:// 开头' })
        });
        byId.settingsSave.fire('click');
        await flush();
        includes(byId.settingsHint.textContent, '源1 地址无效');
        truthy(byId.settingsHint.classList.contains('error'), '应显示错误样式');
        eq(byId.settingsSave.disabled, false);
    });

    await okAsync('网络异常时给出可读提示', async () => {
        global.window.fetch = () => Promise.reject(new Error('network down'));
        byId.settingsSave.fire('click');
        await flush();
        includes(byId.settingsHint.textContent, 'network down');
    });

    console.log('\n共 ' + checks + ' 项断言，失败 ' + fails + ' 项');
    if (fails > 0) {
        process.exit(1);
    }
})();
