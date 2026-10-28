// 窗口化虚拟滚动：数据全量留在内存，DOM 只渲染可视窗口附近的一段行。
// 上下用占位 div 撑出滚动条，滚到边缘时按需扩窗，长日志挂机也不会把 DOM 撑爆。
// 行高不做假设（日志行会折行），用实测平均值校正占位高度。
(function () {
  'use strict';

  const WIN = 2500;   // 窗口内渲染的最大行数
  const EDGE = 700;   // 距离窗口边缘多少像素时扩窗
  const STEP = 800;   // 每次扩窗的行数

  class VScroll {
    constructor(box, opts) {
      opts = opts || {};
      this.box = box;
      this.buildHtml = opts.buildHtml;
      this.emptyHtml = opts.emptyHtml || '<div class="empty">暂无内容</div>';
      this.match = opts.match || null;
      this.winSize = opts.winSize || WIN;
      this.maxLines = opts.maxLines || 30000;
      this.avgH = opts.avgH || 20;
      this.autoScroll = true;
      this.onNearTop = opts.onNearTop || null;
      this.all = [];
      this.visible = [];
      this.winStart = 0;
      this.winEnd = 0;
      this.busy = false;
      this._scroll = this.handleScroll.bind(this);
      if (box) box.addEventListener('scroll', this._scroll, { passive: true });
    }

    filter() {
      if (!this.match) return this.all.slice();
      const out = [];
      for (const l of this.all) if (this.match(l)) out.push(l);
      return out;
    }

    // 整体替换（打开新日志、清屏）
    setLines(lines) {
      this.all = (lines || []).slice();
      this.visible = this.filter();
      this.winStart = Math.max(0, this.visible.length - this.winSize);
      this.winEnd = this.visible.length;
      this.render(false);
    }

    // 追加新行（实时追踪）
    append(lines) {
      if (!lines || !lines.length) return;
      const add = [];
      for (const l of lines) {
        this.all.push(l);
        if (!this.match || this.match(l)) add.push(l);
      }
      if (this.all.length > this.maxLines) this.trimData();
      if (!add.length) { this.updatePads(); return; }

      const atEnd = this.winEnd >= this.visible.length;
      for (const l of add) this.visible.push(l);
      if (atEnd) {
        this.appendNodes(add);
        if (this.autoScroll) this.scrollToBottom();
      } else {
        // 用户正在看历史，不动窗口，只把底部占位撑高
        this.updatePads();
      }
    }

    // 向前插入历史（服务端翻页）
    prepend(lines) {
      if (!lines || !lines.length) return;
      const beforeTop = this.box ? this.box.scrollTop : 0;
      let added = 0;
      for (const l of lines) if (!this.match || this.match(l)) added++;
      this.all = lines.concat(this.all);
      if (this.all.length > this.maxLines) this.all.length = this.maxLines;
      this.visible = this.filter();
      this.winStart += added;
      this.winEnd += added;
      if (this.winEnd > this.visible.length) {
        this.winEnd = this.visible.length;
        this.winStart = Math.max(0, this.winEnd - this.winSize);
      }
      this.render(true);
      if (this.autoScroll) {
        this.scrollToBottom();
      } else if (this.box) {
        // 新行插在窗口上方，整体下移了 added 行的高度，补回滚动位置保持视线不动
        this.busy = true;
        this.box.scrollTop = beforeTop + added * this.avgH;
        this.busy = false;
      }
    }

    // 过滤条件变化
    refilter() {
      this.visible = this.filter();
      if (this.autoScroll) {
        this.setWindowAtTail();
      } else {
        this.winStart = Math.max(0, Math.min(this.winStart, Math.max(0, this.visible.length - this.winSize)));
        this.winEnd = Math.min(this.visible.length, this.winStart + this.winSize);
        this.render(true);
      }
    }

    trimData() {
      const cut = this.all.length - this.maxLines;
      this.all.splice(0, cut);
      this.visible = this.filter();
      this.winStart = Math.max(0, this.visible.length - this.winSize);
      this.winEnd = this.visible.length;
      this.render(false);
    }

    render(keepScroll) {
      const box = this.box;
      if (!box) return;
      this.busy = true;
      const prevTop = keepScroll ? box.scrollTop : 0;
      const n = this.visible.length;
      if (!n) {
        box.innerHTML = this.emptyHtml;
        this.winStart = this.winEnd = 0;
        this.busy = false;
        return;
      }
      const maxStart = Math.max(0, n - this.winSize);
      let start = keepScroll ? this.winStart : maxStart;
      if (start > maxStart) start = maxStart;
      if (start < 0) start = 0;
      const end = Math.min(n, start + this.winSize);
      this.winStart = start;
      this.winEnd = end;

      const inner = this.buildHtml(this.visible.slice(start, end));
      box.innerHTML =
        '<div class="vs-pad vs-pad-top" style="height:' + (start * this.avgH) + 'px"></div>' +
        inner +
        '<div class="vs-pad vs-pad-bottom" style="height:' + ((n - end) * this.avgH) + 'px"></div>';

      this.measure();
      if (keepScroll) {
        box.scrollTop = prevTop;
      } else if (this.autoScroll) {
        box.scrollTop = box.scrollHeight;
      }
      this.busy = false;
    }

    // 用实测内容高度校正平均行高，再回写占位高度
    measure() {
      const box = this.box;
      const count = box.querySelectorAll('.log-line').length;
      if (count <= 0) return;
      const top = box.querySelector('.vs-pad-top');
      const bot = box.querySelector('.vs-pad-bottom');
      const pads = (top ? top.offsetHeight : 0) + (bot ? bot.offsetHeight : 0);
      const content = box.scrollHeight - pads;
      if (content <= 0) return;
      const avg = content / count;
      // 行高只可能在十几到几十像素之间，超出范围说明测量时机不对，直接丢弃
      if (!(avg >= 8 && avg <= 120)) return;
      this.avgH = this.avgH * 0.6 + avg * 0.4;
      if (top) top.style.height = (this.winStart * this.avgH) + 'px';
      if (bot) bot.style.height = Math.max(0, this.visible.length - this.winEnd) * this.avgH + 'px';
    }

    updatePads() {
      const box = this.box;
      if (!box) return;
      const top = box.querySelector('.vs-pad-top');
      const bot = box.querySelector('.vs-pad-bottom');
      if (top) top.style.height = (this.winStart * this.avgH) + 'px';
      if (bot) bot.style.height = Math.max(0, this.visible.length - this.winEnd) * this.avgH + 'px';
    }

    appendNodes(lines) {
      const box = this.box;
      if (!box) return;
      const placeholder = box.querySelector('.empty, .empty-state');
      if (placeholder) placeholder.remove();
      const bot = box.querySelector('.vs-pad-bottom');
      const holder = document.createElement('div');
      holder.innerHTML = this.buildHtml(lines);
      const ref = bot || null;
      while (holder.firstChild) box.insertBefore(holder.firstChild, ref);
      this.winEnd += lines.length;
      this.updatePads();
      this.trimTop();
    }

    // 自动滚动时窗口会一直往上涨，超过上限就从顶部裁掉（下方用占位撑住）
    trimTop() {
      const limit = this.winSize + 400;
      if (this.winEnd - this.winStart <= limit) return;
      const padTop = this.box.querySelector('.vs-pad-top');
      let guard = 0;
      while (this.winEnd - this.winStart > this.winSize && guard++ < 20000) {
        // 顶部第一行要从占位之后开始找，否则永远撞到 .vs-pad-top
        const el = padTop ? padTop.nextElementSibling : this.box.firstElementChild;
        if (!el || el.classList.contains('vs-pad')) break;
        el.remove();
        this.winStart++;
      }
      this.updatePads();
    }

    setWindowAtTail() {
      const n = this.visible.length;
      this.winStart = Math.max(0, n - this.winSize);
      this.winEnd = n;
      this.render(false);
    }

    scrollToBottom() {
      if (this.winEnd < this.visible.length) {
        this.setWindowAtTail();
        return;
      }
      this.busy = true;
      this.box.scrollTop = this.box.scrollHeight;
      this.busy = false;
    }

    handleScroll() {
      if (this.busy) return;
      const box = this.box;
      const top = box.querySelector('.vs-pad-top');
      const bot = box.querySelector('.vs-pad-bottom');
      const topH = top ? top.offsetHeight : 0;
      const botH = bot ? bot.offsetHeight : 0;

      if (this.winStart > 0 && box.scrollTop < topH + EDGE) {
        this.extendBack();
        return;
      }
      if (this.winEnd < this.visible.length &&
          box.scrollTop + box.clientHeight > box.scrollHeight - botH - EDGE) {
        this.extendForward();
        return;
      }
      if (this.winStart === 0 && box.scrollTop <= 50 && this.onNearTop) this.onNearTop();
    }

    extendBack() {
      const step = Math.min(STEP, this.winStart);
      if (step <= 0) return;
      this.winStart -= step;
      this.winEnd = Math.min(this.visible.length, this.winStart + this.winSize);
      this.render(true);
    }

    extendForward() {
      const step = Math.min(STEP, this.visible.length - this.winEnd);
      if (step <= 0) return;
      this.winStart += step;
      this.winEnd += step;
      this.render(true);
    }

    // 是否在底部附近
    nearBottom(slack) {
      const box = this.box;
      const bot = box.querySelector('.vs-pad-bottom');
      const botH = bot ? bot.offsetHeight : 0;
      return box.scrollHeight - box.scrollTop - box.clientHeight - botH <= (slack || 10);
    }
  }

  window.VScroll = VScroll;
})();
