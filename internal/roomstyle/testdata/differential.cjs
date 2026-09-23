// Sanitizer output, parsed by Chromium's own CSS parser. Driven by
// TestSanitizedCSSParsesAlikeInBrowser (browser_test.go), which writes a JSON
// file of {scope, cases: [{in, out, rules}]}: generated hostile inputs and what
// SanitizeWith made of them. The sanitizer re-parses its own output; this checks
// that the browser reads the same thing, so no tokenizer or parser disagreement
// (escapes, comments, strings, blocks, nesting) can smuggle a rule, a selector
// outside the room's scope, a fetch, or a page-zone property past it.
const {chromium} = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const fs = require('node:fs');

(async () => {
  const data = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
  const browser = await chromium.launch({headless: true, ...(process.env.CHROMIUM_PATH ? {executablePath: process.env.CHROMIUM_PATH} : {})});
  try {
    const page = await browser.newPage();
    await page.goto('about:blank');
    const bad = await page.evaluate(({scope, cases}) => {
      const bad = [];
      const ruleTypes = ['CSSStyleRule', 'CSSMediaRule', 'CSSSupportsRule', 'CSSContainerRule', 'CSSLayerBlockRule',
        'CSSLayerStatementRule', 'CSSKeyframesRule', 'CSSKeyframeRule', 'CSSFontFaceRule'];
      // Page-zone properties that could hide, move, stack, clip or draw (zones.go).
      const pageDenied = /^(position|z-index|top|left|right|bottom|inset.*|transform.*|translate|rotate|scale|perspective.*|zoom|opacity|filter|backdrop-filter|mix-blend-mode|isolation|will-change|clip|clip-path|mask.*|contain|contain-intrinsic.*|content-visibility|visibility|content|overflow.*|line-clamp|text-indent|height|max-height|block-size|max-block-size|aspect-ratio|order|direction|unicode-bidi|writing-mode|text-orientation|animation.*|offset.*|list-style.*|counter-.*|quotes|hyphenate-character|columns|column-count|column-width|column-span|break-.*|grid-(row|column|area)(-.*)?|grid-template-areas|anchor-.*|position-.*|view-transition-.*|container|container-type)$/;
      // Drop what cannot load anything before looking for url() and friends:
      // the allowed URL forms and strings go; escaped name characters outside
      // strings are decoded, since u\72l( is url( to a tokenizer.
      const inert = value => {
        value = value.replace(/url\("(\/a\/[0-9a-f]{32}|data:image\/(png|jpeg|gif|webp);base64,[A-Za-z0-9+/=]*)"\)/g, 'ALLOWED');
        let out = '';
        for (let i = 0; i < value.length; i++) {
          const ch = value[i];
          if (ch === '\\') {
            const hex = /^[0-9a-fA-F]{1,6}\s?/.exec(value.slice(i + 1));
            const decoded = hex ? String.fromCodePoint(Math.min(parseInt(hex[0], 16), 0x10ffff) || 0xfffd) : value[i + 1] || '';
            // An escaped name character stays itself; escaped punctuation, such
            // as \( in a serialized animation name, is not syntax.
            out += /^[\w-]$|^[^\x00-\x7f]/u.test(decoded) ? decoded : '_';
            i += hex ? hex[0].length : 1;
          } else if (ch === '"' || ch === "'") {
            for (i++; i < value.length && value[i] !== ch; i++) if (value[i] === '\\') i++;
            out += '_';
          } else out += ch;
        }
        return out;
      };
      // Top-level selectors: commas inside :is() and friends do not separate.
      const selectors = text => {
        const list = [];
        let depth = 0, start = 0, quote = '';
        for (let i = 0; i < text.length; i++) {
          const ch = text[i];
          if (ch === '\\') { i++; continue; }
          if (quote) { if (ch === quote) quote = ''; continue; }
          if (ch === '"' || ch === "'") quote = ch;
          else if (ch === '(' || ch === '[') depth++;
          else if (ch === ')' || ch === ']') depth--;
          else if (ch === ',' && depth === 0) { list.push(text.slice(start, i)); start = i + 1; }
        }
        return [...list, text.slice(start)];
      };
      for (const c of cases) {
        const sheet = new CSSStyleSheet();
        try { sheet.replaceSync(c.out); } catch (error) { bad.push(['unparsable', String(error), c.in, c.out]); continue; }
        let count = 0;
        const walk = rules => {
          for (const rule of rules) {
            count++;
            const type = rule.constructor.name;
            if (!ruleTypes.includes(type)) bad.push(['rule type', type, c.in, c.out]);
            if (type === 'CSSStyleRule') {
              if (!selectors(rule.selectorText).every(s => s.trim().startsWith('.' + scope))) bad.push(['selector', rule.selectorText, c.in, c.out]);
              if (rule.cssRules && rule.cssRules.length) bad.push(['nested rule', rule.cssText, c.in, c.out]);
              const afterBody = rule.selectorText.split('.room-body').slice(1).join('');
              const pageZone = !rule.selectorText.includes('.room-body') || /[~+]/.test(afterBody);
              for (let i = 0; i < rule.style.length; i++) {
                const prop = rule.style[i], value = inert(rule.style.getPropertyValue(prop));
                if (/(^|[^\w\-\u0080-\u{10ffff}])url\(/iu.test(value)) bad.push(['url', prop, value, c.in, c.out]);
                if (/(^|[^\w\-\u0080-\u{10ffff}])(-webkit-)?(image-set|image|cross-fade|element|src|paint)\(/iu.test(value)) bad.push(['loading function', prop, value, c.in, c.out]);
                if (pageZone && pageDenied.test(prop) && !(prop === 'height' && value === 'auto')) bad.push(['page-zone property', prop, value, rule.selectorText, c.in, c.out]);
                if (pageZone && prop.startsWith('-') && !prop.startsWith('--')) bad.push(['page-zone vendor property', prop, c.in, c.out]);
              }
            }
            if (type === 'CSSFontFaceRule') {
              const src = rule.style.getPropertyValue('src');
              if (src && !/^url\("\/a\/[0-9a-f]{32}"\)( format\("[a-z0-9]+"\))?$/.test(src)) bad.push(['font src', src, c.in, c.out]);
            }
            if (rule.cssRules) walk(rule.cssRules);
          }
        };
        walk(sheet.cssRules);
        // More rules in the browser than the sanitizer counted means the two
        // disagree about where a string, comment or block ends.
        if (count > c.rules) bad.push(['rule count', count, c.rules, c.in, c.out]);
      }
      return bad;
    }, data);
    const seen = new Set();
    for (const finding of bad) {
      const key = finding[0] + ' ' + finding[1];
      if (!seen.has(key)) { seen.add(key); console.log(JSON.stringify(finding)); }
    }
    console.log(`${data.cases.length} sanitized stylesheets, ${bad.length} disagreements`);
    process.exitCode = bad.length ? 1 : 0;
  } finally {
    await browser.close();
  }
})().catch(error => { console.error(error); process.exit(1); });
