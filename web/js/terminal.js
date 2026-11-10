// xterm 视图封装：主题、自适应、搜索、字体缩放
export const THEMES = {
  dark: {
    background: '#14161c',
    foreground: '#d7dbe0',
    cursor: '#7ee787',
    cursorAccent: '#14161c',
    selectionBackground: 'rgba(110,160,255,0.28)',
    black: '#1b1e26',
    red: '#ff6b6b',
    green: '#7ee787',
    yellow: '#f2cc60',
    blue: '#6cb6ff',
    magenta: '#c792ea',
    cyan: '#56d4dd',
    white: '#d7dbe0',
    brightBlack: '#5b6470',
    brightRed: '#ff8b8b',
    brightGreen: '#a5f3a5',
    brightYellow: '#ffd97a',
    brightBlue: '#8cc8ff',
    brightMagenta: '#dbb0ff',
    brightCyan: '#7fe6ec',
    brightWhite: '#f2f5f9',
  },
  light: {
    background: '#fbfbfd',
    foreground: '#24292f',
    cursor: '#0a7d4f',
    cursorAccent: '#fbfbfd',
    selectionBackground: 'rgba(56,120,255,0.22)',
    black: '#24292f',
    red: '#c02b2b',
    green: '#0a7d4f',
    yellow: '#9a6700',
    blue: '#0a5bd3',
    magenta: '#8250df',
    cyan: '#0d7b8a',
    white: '#6e7781',
    brightBlack: '#8c959f',
    brightRed: '#e5534b',
    brightGreen: '#1a7f37',
    brightYellow: '#bf8700',
    brightBlue: '#2188ff',
    brightMagenta: '#a475f9',
    brightCyan: '#1b9aaa',
    brightWhite: '#24292f',
  },
};

export const FONT_STACK =
  '"JetBrains Mono", "Fira Code", "Cascadia Mono", "DejaVu Sans Mono", ' +
  '"Noto Sans Mono CJK SC", "Source Han Mono SC", "WenQuanYi Micro Hei Mono", ' +
  'Menlo, Consolas, monospace';

export class TermView {
  constructor(theme, fontSize) {
    this.themeName = theme || 'dark';
    this.fontSize = fontSize || 14;
    this.term = new window.Terminal({
      fontFamily: FONT_STACK,
      fontSize: this.fontSize,
      lineHeight: 1.22,
      letterSpacing: 0,
      cursorBlink: true,
      cursorStyle: 'bar',
      scrollback: 20000,
      allowProposedApi: true,
      macOptionIsMeta: true,
      rightClickSelectsWord: true,
      theme: THEMES[this.themeName],
      drawBoldTextInBrightColors: true,
      minimumContrastRatio: 1,
    });
    this.fitAddon = new window.FitAddon.FitAddon();
    this.searchAddon = new window.SearchAddon.SearchAddon();
    this.term.loadAddon(this.fitAddon);
    this.term.loadAddon(this.searchAddon);
    try {
      this.term.loadAddon(new window.WebLinksAddon.WebLinksAddon());
    } catch { /* 可选插件，失败不影响使用 */ }
    this.onData = null;
    this.onResize = null;
    this.onTitle = null;
    this.term.onData((d) => this.onData && this.onData(d));
    this.term.onResize(({ cols, rows }) => this.onResize && this.onResize(cols, rows));
    this.term.onTitleChange((t) => this.onTitle && this.onTitle(t));
  }

  mount(container) {
    this.container = container;
    this.term.open(container);
    // 容器尚未显示（display:none）时不要 fit，否则会以极小的列宽触发 resize，
    // 导致 shell 按 2 列重绘提示符并在回放里留下碎裂的行。
    if (container.clientWidth > 0) this.fit();
  }

  fit() {
    try {
      this.fitAddon.fit();
    } catch { /* 容器尚未布局 */ }
  }

  dims() {
    return { cols: this.term.cols, rows: this.term.rows };
  }

  write(bytes) {
    this.term.write(bytes);
  }

  writeText(text) {
    this.term.write(text);
  }

  reset() {
    this.term.reset();
  }

  clear() {
    this.term.clear();
  }

  search(term, dir) {
    if (!term) return false;
    return dir === 'prev'
      ? this.searchAddon.findPrevious(term, { caseSensitive: false })
      : this.searchAddon.findNext(term, { caseSensitive: false });
  }

  setTheme(name) {
    this.themeName = name;
    this.term.options.theme = THEMES[name] || THEMES.dark;
  }

  setFontSize(size) {
    this.fontSize = Math.max(9, Math.min(28, size));
    this.term.options.fontSize = this.fontSize;
    this.fit();
  }

  focus() {
    this.term.focus();
  }

  dispose() {
    try {
      this.term.dispose();
    } catch { /* ignore */ }
  }
}
