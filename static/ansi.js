// ANSI 转义解析：把日志里的颜色/样式转义渲染成 HTML，其余控制序列一律丢弃。
// 日志内容一律先做 HTML 转义，避免日志里的 <script> 之类注入页面。
// 深色背景下终端标准色 30-37 偏暗，这里换成一套可读性更好的调色板。
(function () {
  'use strict';

  const FG = {
    30: '#6e7681', 31: '#ff7b72', 32: '#7ee787', 33: '#e3b341',
    34: '#79c0ff', 35: '#d2a8ff', 36: '#56d4dd', 37: '#c9d1d9',
    90: '#8b949e', 91: '#ffa198', 92: '#aff5b4', 93: '#f2cc60',
    94: '#a5d6ff', 95: '#e2c5ff', 96: '#9be9f0', 97: '#f0f6fc'
  };
  const BG = {
    40: 'rgba(110,118,129,0.25)', 41: 'rgba(248,81,73,0.30)', 42: 'rgba(63,185,80,0.28)',
    43: 'rgba(210,153,29,0.30)', 44: 'rgba(56,139,253,0.30)', 45: 'rgba(188,140,255,0.28)',
    46: 'rgba(57,197,207,0.28)', 47: 'rgba(201,209,217,0.16)',
    100: 'rgba(139,148,158,0.25)', 101: 'rgba(255,123,114,0.30)', 102: 'rgba(126,231,135,0.25)',
    103: 'rgba(242,204,96,0.25)', 104: 'rgba(165,214,255,0.25)', 105: 'rgba(226,197,255,0.25)',
    106: 'rgba(155,233,240,0.25)', 107: 'rgba(240,246,252,0.20)'
  };

  // 去掉所有转义序列，只留可见文本（等级判定用，避免转义码干扰行首匹配）
  const ESC_RE = /\x1b\[[0-9;?]*[ -\/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b./g;
  function strip(s) {
    return s.indexOf('\x1b') === -1 ? s : s.replace(ESC_RE, '');
  }

  function esc(s) {
    return s.replace(/[&<>"']/g, function (ch) {
      switch (ch) {
        case '&': return '&amp;';
        case '<': return '&lt;';
        case '>': return '&gt;';
        case '"': return '&quot;';
        default: return '&#39;';
      }
    });
  }

  // 256 色：0-15 走基础色，16-231 是 6×6×6 色立方，232-255 是灰阶
  function xterm256(n) {
    if (!(n >= 0 && n < 256)) return null;
    if (n < 8) return FG[30 + n];
    if (n < 16) return FG[90 + (n - 8)];
    if (n < 232) {
      const i = n - 16;
      const r = Math.floor(i / 36), g = Math.floor((i % 36) / 6), b = i % 6;
      const f = function (v) { return v === 0 ? 0 : 55 + v * 40; };
      return 'rgb(' + f(r) + ',' + f(g) + ',' + f(b) + ')';
    }
    const v = 8 + (n - 232) * 10;
    return 'rgb(' + v + ',' + v + ',' + v + ')';
  }

  function applySGR(st, params) {
    const codes = params === '' ? [0] : params.split(';').map(function (x) {
      return x === '' ? 0 : parseInt(x, 10) || 0;
    });
    for (let k = 0; k < codes.length; k++) {
      const c = codes[k];
      if (c === 0) {
        st.color = st.bg = null; st.bold = st.dim = st.italic = st.underline = false;
      } else if (c === 1) st.bold = true;
      else if (c === 2) st.dim = true;
      else if (c === 3) st.italic = true;
      else if (c === 4) st.underline = true;
      else if (c === 7) { const t = st.color; st.color = st.bg; st.bg = t; }
      else if (c === 22) { st.bold = false; st.dim = false; }
      else if (c === 23) st.italic = false;
      else if (c === 24) st.underline = false;
      else if (c === 39) st.color = null;
      else if (c === 49) st.bg = null;
      else if ((c >= 30 && c <= 37) || (c >= 90 && c <= 97)) st.color = FG[c] || st.color;
      else if ((c >= 40 && c <= 47) || (c >= 100 && c <= 107)) st.bg = BG[c] || st.bg;
      else if (c === 38 || c === 48) {
        const key = c === 38 ? 'color' : 'bg';
        if (codes[k + 1] === 5) {
          const v = xterm256(codes[k + 2]);
          if (v) st[key] = v;
          k += 2;
        } else if (codes[k + 1] === 2) {
          const r = codes[k + 2] | 0, g = codes[k + 3] | 0, b = codes[k + 4] | 0;
          st[key] = 'rgb(' + r + ',' + g + ',' + b + ')';
          k += 4;
        }
      }
    }
  }

  function styleOf(st) {
    const parts = [];
    if (st.color) parts.push('color:' + st.color);
    if (st.bg) parts.push('background:' + st.bg);
    if (st.bold) parts.push('font-weight:700');
    if (st.dim) parts.push('opacity:.65');
    if (st.italic) parts.push('font-style:italic');
    if (st.underline) parts.push('text-decoration:underline');
    return parts.join(';');
  }

  let highlightRe = null, highlightKey = '';

  function textHtml(s, filter) {
    let html = esc(s);
    if (filter) {
      if (filter !== highlightKey) {
        highlightKey = filter;
        highlightRe = new RegExp('(' + filter.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + ')', 'gi');
      }
      html = html.replace(highlightRe, '<span class="hl">$1</span>');
    }
    return html;
  }

  // 把一行日志渲染成 HTML 片段（返回 innerHTML，调用方自己套 .log-line 容器）
  function render(line, filter) {
    if (line.indexOf('\x1b') === -1) {
      return textHtml(line.replace(/\r/g, ''), filter);
    }
    let out = '';
    const st = { color: null, bg: null, bold: false, dim: false, italic: false, underline: false };
    let buf = '';
    let i = 0;
    const n = line.length;

    const flush = function () {
      if (!buf) return;
      const html = textHtml(buf, filter);
      const css = styleOf(st);
      out += css ? '<span style="' + css + '">' + html + '</span>' : html;
      buf = '';
    };

    while (i < n) {
      const ch = line.charCodeAt(i);
      if (ch === 13) { i++; continue; }              // 丢掉 \r
      if (ch !== 27) { buf += line[i]; i++; continue; }

      flush();
      const next = line[i + 1];
      if (next === '[') {
        const m = /^\x1b\[([0-9;]*)([ -\/]*)([@-~])/.exec(line.slice(i, i + 64));
        if (!m) { i++; continue; }
        if (m[3] === 'm') applySGR(st, m[1]);
        // 其它 CSI（清屏、光标移动等）直接丢弃，不显示
        i += m[0].length;
      } else if (next === ']') {
        // OSC ... BEL 或 ESC \
        let j = i + 2;
        while (j < n) {
          if (line.charCodeAt(j) === 7) { j++; break; }
          if (line.charCodeAt(j) === 27 && line[j + 1] === '\\') { j += 2; break; }
          j++;
        }
        i = j;
      } else {
        i += 2;
      }
    }
    flush();
    return out;
  }

  window.Ansi = { render: render, strip: strip, esc: esc };
})();
