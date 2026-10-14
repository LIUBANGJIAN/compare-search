'use strict';

/*
 * panel.js 的单元测试（Node 直接运行，无第三方依赖）：
 *   node web/panel.test.js
 * 退出码非 0 表示有用例失败。
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

function eq(actual, expected, label) {
    const a = JSON.stringify(actual);
    const e = JSON.stringify(expected);
    if (a !== e) throw new Error((label || '值不相等') + '：实际 ' + a + '，期望 ' + e);
}

console.log('== splitKeywords：切分与去重');

ok('空输入返回空数组', () => {
    eq(api.splitKeywords(''), []);
    eq(api.splitKeywords('   '), []);
    eq(api.splitKeywords('\n\t '), []);
    eq(api.splitKeywords(null), []);
    eq(api.splitKeywords(undefined), []);
});

ok('单个关键字原样返回', () => {
    eq(api.splitKeywords('SSIS-001'), ['SSIS-001']);
    eq(api.splitKeywords('  少林足球  '), ['少林足球']);
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

ok('空格分隔', () => {
    eq(api.splitKeywords('one two   three'), ['one', 'two', 'three']);
});

ok('换行分隔（多行粘贴）', () => {
    eq(api.splitKeywords('第一行\n第二行\n\n第三行\n'), ['第一行', '第二行', '第三行']);
});

ok('混合分隔符', () => {
    eq(api.splitKeywords(' a, b；c\n d\t\n'), ['a', 'b', 'c', 'd']);
});

ok('重复关键字去重且保持首次出现顺序', () => {
    eq(api.splitKeywords('b a b a c'), ['b', 'a', 'c']);
    eq(api.splitKeywords('A a'), ['A', 'a']);
});

ok('全角空格与制表符也算分隔符', () => {
    eq(api.splitKeywords('x\u3000y'), ['x', 'y']);
    eq(api.splitKeywords('x\t\ty'), ['x', 'y']);
});

ok('大小写与连字符不被改写', () => {
    eq(api.splitKeywords('ABP-123,abc-456'), ['ABP-123', 'abc-456']);
});

ok('单个关键字含前导/尾随空白时被裁剪', () => {
    eq(api.splitKeywords('  ABP-123  '), ['ABP-123']);
});

console.log('== buildUrls：两个源的地址构造');

ok('中文关键字做 URL 编码', () => {
    const u = api.buildUrls('中文', 'http://127.0.0.1:18898');
    eq(u.source1, 'http://127.0.0.1:18898/files?q=' + encodeURIComponent('中文'));
    eq(u.source2, '/s?q=' + encodeURIComponent('中文'));
});

ok('特殊字符被编码，不会破坏查询串', () => {
    const u = api.buildUrls('a&b=c d', 'http://h:1');
    eq(u.source1, 'http://h:1/files?q=' + encodeURIComponent('a&b=c d'));
    if (u.source2.indexOf('&') !== -1) throw new Error('源2 地址未编码 & ：' + u.source2);
    if (u.source2.indexOf(' ') !== -1) throw new Error('源2 地址未编码空格：' + u.source2);
});

ok('源2 始终是同源相对路径', () => {
    const u = api.buildUrls('x', 'http://127.0.0.1:1');
    if (!u.source2.startsWith('/s?q=')) throw new Error('源2 不是相对路径：' + u.source2);
});

console.log('== 翻页索引：边界行为');

ok('nextIndex 在末尾保持不动', () => {
    eq(api.nextIndex(0, 3), 1);
    eq(api.nextIndex(2, 3), 2);
    eq(api.nextIndex(0, 1), 0);
});

ok('prevIndex 在开头保持不动', () => {
    eq(api.prevIndex(2, 3), 1);
    eq(api.prevIndex(0, 3), 0);
    eq(api.prevIndex(0, 1), 0);
});

ok('空列表时索引归零', () => {
    eq(api.nextIndex(5, 0), 0);
    eq(api.prevIndex(5, 0), 0);
});

ok('indexOfKeyword 命中与未命中', () => {
    eq(api.indexOfKeyword(['a', 'b'], 'b'), 1);
    eq(api.indexOfKeyword(['a', 'b'], 'z'), -1);
    eq(api.indexOfKeyword([], 'a'), -1);
});

ok('多关键字完整翻页序列走一遍', () => {
    const kws = api.splitKeywords('A B C');
    eq(kws, ['A', 'B', 'C']);
    let i = 0;
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

console.log('== 契约：与后端注入的占位符保持一致');

ok('源1 搜索路径与 panel.html 的 data-q1 前缀一致', () => {
    eq(api.SOURCE1_SEARCH_PATH, '/files');
});

ok('源2 搜索路径与 Go 侧生成的 data-q2 前缀一致', () => {
    eq(api.SOURCE2_SEARCH_PATH, '/s');
});

// ---------------------------------------------------------------------------
// DOM 级行为测试：真正驱动「多关键字 → 翻页 → 两个 iframe 同步」这条链路
// ---------------------------------------------------------------------------

const P1_PORT = 18898;
const P1_BASE = 'http://127.0.0.1:' + P1_PORT;

function makeEl(id, tag) {
    const el = {
        id: id,
        tagName: (tag || 'div').toUpperCase(),
        src: '',
        hidden: false,
        disabled: false,
        textContent: '',
        value: '',
        attrs: Object.create(null),
        listeners: Object.create(null),
        classes: Object.create(null),
        getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
        setAttribute(k, v) { this.attrs[k] = String(v); },
        addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); },
        fire(t, ev) {
            (this.listeners[t] || []).slice().forEach((fn) => {
                fn(ev || { preventDefault() {} });
            });
        }
    };
    el.classList = {
        add(c) { el.classes[c] = true; },
        contains(c) { return !!el.classes[c]; }
    };
    return el;
}

const els = {};
['frame1', 'frame2', 'kwInput', 'searchBtn', 'prevBtn', 'nextBtn', 'pager',
    'pagerInfo', 'pagerKw', 'clearBtn'].forEach((id) => {
    const tag = id.indexOf('frame') === 0 ? 'iframe' : (id === 'kwInput' ? 'textarea' : 'button');
    els[id] = makeEl(id, tag);
});

const dataLines = [makeEl('', 'a'), makeEl('', 'a')];
dataLines[0].textContent = 'ABP-123';
dataLines[0].setAttribute('data-id', 'item-0');
dataLines[1].textContent = 'SSIS-001';
dataLines[1].setAttribute('data-id', 'item-1');

const docListeners = Object.create(null);
const localStore = Object.create(null);

global.document = {
    readyState: 'complete',
    body: { getAttribute: (k) => (k === 'data-p1-port' ? String(P1_PORT) : null) },
    getElementById: (id) => els[id] || null,
    querySelectorAll: (sel) => (sel === '.data-line' ? dataLines : []),
    addEventListener(t, fn) { (docListeners[t] = docListeners[t] || []).push(fn); },
    fire(t, ev) {
        (docListeners[t] || []).slice().forEach((fn) => fn(ev || { preventDefault() {} }));
    }
};
global.window = { location: { protocol: 'http:', hostname: '127.0.0.1', port: '18899' } };
global.localStorage = {
    getItem: (k) => (k in localStore ? localStore[k] : null),
    setItem: (k, v) => { localStore[k] = String(v); }
};

api.init();

const key = () => (ev) => ev;

console.log('== DOM 行为：初始化状态');

ok('初始不显示翻页器', () => {
    eq(els.pager.hidden, true);
});

ok('初始两个 iframe 都是空白页', () => {
    eq(els.frame1.src, '');
    eq(els.frame2.src, '');
});

console.log('== DOM 行为：单个关键字');

ok('输入单个关键字后点搜索，两个 iframe 同时跳转', () => {
    els.kwInput.value = 'ABP-123';
    els.searchBtn.fire('click');
    eq(els.frame1.src, P1_BASE + '/files?q=ABP-123');
    eq(els.frame2.src, '/s?q=ABP-123');
});

ok('单关键字时翻页器显示 1 / 1 且两个按钮都禁用', () => {
    eq(els.pager.hidden, false);
    eq(els.pagerInfo.textContent, '1 / 1');
    eq(els.pagerKw.textContent, 'ABP-123');
    eq(els.prevBtn.disabled, true);
    eq(els.nextBtn.disabled, true);
});

console.log('== DOM 行为：多个关键字 + 翻页');

ok('三个关键字默认停在第一条', () => {
    els.kwInput.value = '甲 乙 丙';
    els.searchBtn.fire('click');
    eq(els.pagerInfo.textContent, '1 / 3');
    eq(els.pagerKw.textContent, '甲');
    eq(els.frame1.src, P1_BASE + '/files?q=' + encodeURIComponent('甲'));
    eq(els.frame2.src, '/s?q=' + encodeURIComponent('甲'));
    eq(els.prevBtn.disabled, true);
    eq(els.nextBtn.disabled, false);
});

ok('点「下一个」两个 iframe 同步切到第二个关键字', () => {
    els.nextBtn.fire('click');
    eq(els.pagerInfo.textContent, '2 / 3');
    eq(els.pagerKw.textContent, '乙');
    eq(els.frame1.src, P1_BASE + '/files?q=' + encodeURIComponent('乙'));
    eq(els.frame2.src, '/s?q=' + encodeURIComponent('乙'));
});

ok('到末尾后「下一个」按钮禁用且不再越界', () => {
    els.nextBtn.fire('click');
    eq(els.pagerInfo.textContent, '3 / 3');
    eq(els.pagerKw.textContent, '丙');
    eq(els.nextBtn.disabled, true);
    els.nextBtn.fire('click');
    eq(els.pagerInfo.textContent, '3 / 3');
});

ok('点「上一个」回到第二条', () => {
    els.prevBtn.fire('click');
    eq(els.pagerInfo.textContent, '2 / 3');
    eq(els.pagerKw.textContent, '乙');
});

ok('Alt+← 快捷键翻页', () => {
    global.document.fire('keydown', { altKey: true, key: 'ArrowLeft', preventDefault() {} });
    eq(els.pagerInfo.textContent, '1 / 3');
    eq(els.pagerKw.textContent, '甲');
});

ok('Alt+→ 快捷键翻页', () => {
    global.document.fire('keydown', { altKey: true, key: 'ArrowRight', preventDefault() {} });
    eq(els.pagerInfo.textContent, '2 / 3');
});

ok('Ctrl+Enter 触发搜索', () => {
    els.kwInput.value = 'X,Y';
    els.kwInput.fire('keydown', { key: 'Enter', ctrlKey: true, preventDefault() {} });
    eq(els.pagerInfo.textContent, '1 / 2');
    eq(els.pagerKw.textContent, 'X');
});

console.log('== DOM 行为：列表点击与清空');

ok('点番号列表项立即检索该项并回填输入框', () => {
    dataLines[1].fire('click');
    eq(els.kwInput.value, 'SSIS-001');
    eq(els.pagerInfo.textContent, '1 / 1');
    eq(els.frame1.src, P1_BASE + '/files?q=SSIS-001');
    eq(els.frame2.src, '/s?q=SSIS-001');
});

ok('已点击的列表项被标记并写入 localStorage', () => {
    eq(dataLines[1].classList.contains('clicked'), true);
    const saved = JSON.parse(localStore['clicked_data_items_18899'] || '[]');
    eq(saved, ['item-1']);
});

ok('清空结果会复位 iframe 与翻页器', () => {
    els.clearBtn.fire('click');
    eq(els.pager.hidden, true);
    eq(els.frame1.src, 'about:blank');
    eq(els.frame2.src, 'about:blank');
});

ok('清空后快捷键不再翻页（无关键字时不报错）', () => {
    global.document.fire('keydown', { altKey: true, key: 'ArrowRight', preventDefault() {} });
    eq(els.pager.hidden, true);
});

console.log('\n共 ' + checks + ' 项断言，失败 ' + fails + ' 项');
if (fails > 0) {
    process.exit(1);
}
