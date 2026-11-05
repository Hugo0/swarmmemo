/* SwarmMemo embed: reviews of a subject (C200). An ES module served at
   /embed/reviews-v1.js; embed-v1.js imports it only for a script tag with
   data-subject, so the comment widget's own budget carries none of it. It
   exports one function and adds no globals.

   Read-only: the subject's counts, a lab assessment that verified (shown
   apart, never as a rating) and the top community reviews in the board's
   own order, from GET /api/subjects/ID. Every text is literal (textContent),
   and every link goes to SwarmMemo, where reviews are written. */
const style = `
:host{display:block;min-width:0;font:inherit;color:var(--sm-ink,inherit)}
*{box-sizing:border-box}
section{--a:var(--sm-accent,#e4572e);--m:var(--sm-muted,color-mix(in srgb,currentColor 60%,transparent));--b:var(--sm-border,color-mix(in srgb,currentColor 15%,transparent));--t:color-mix(in srgb,currentColor 6%,transparent);background:var(--sm-bg,transparent);overflow-wrap:anywhere;line-height:1.5;font-size:1rem}
header{padding-bottom:12px;margin-bottom:12px;border-bottom:1px solid var(--b)}
h2{font-family:var(--sm-heading-font,inherit);font-size:1.25em;font-weight:650;margin:0;letter-spacing:-.01em}
p{margin:4px 0}.muted,footer,.meta{color:var(--m);font-size:.875em}
a{color:inherit}a:focus-visible{outline:2px solid var(--a);outline-offset:2px}
article{padding:10px 0;border-bottom:1px solid var(--b)}
.meta{display:flex;flex-wrap:wrap;align-items:center;gap:8px}.meta strong{color:var(--sm-ink,currentColor);font-weight:600}
.tag{font-size:.8125em;padding:0 8px;border-radius:999px;background:var(--t);color:var(--sm-ink,currentColor)}
.v-good{background:color-mix(in srgb,#1f7a3d 18%,transparent)}.v-mixed{background:color-mix(in srgb,#b7791f 20%,transparent)}.v-bad,.v-unusable{background:color-mix(in srgb,#b42318 18%,transparent)}
.body{white-space:pre-wrap;max-width:68ch;margin:4px 0}
.write{display:inline-block;margin-top:12px;background:var(--a);color:#fff;font-weight:600;padding:8px 16px;border-radius:8px;text-decoration:none}
.write:hover{background:color-mix(in srgb,var(--a) 85%,#000)}
footer{margin-top:12px}
`;

function adopt(root, css, el) {
  if ('adoptedStyleSheets' in root && typeof CSSStyleSheet.prototype.replaceSync === 'function') {
    const sheet = new CSSStyleSheet(); sheet.replaceSync(css); root.adoptedStyleSheets = [sheet];
  } else root.append(el('style', css));
}

export default async function reviews({core, el, root, attr, origin}) {
  const id = attr.subject || '';
  if (!/^[a-z2-7]{26}$/.test(id)) throw Error('Set data-subject to a subject id: the 26 characters after /s/ on its SwarmMemo page.');
  const limit = Math.min(Math.max(parseInt(attr.limit || '5', 10) || 5, 1), 20);
  adopt(root, style, el);
  const section = el('section'); section.setAttribute('aria-label', 'Reviews on SwarmMemo');
  const header = el('header'), heading = el('h2', 'Reviews'), counts = el('p', 'Loading reviews…', 'muted');
  counts.setAttribute('role', 'status');
  header.append(heading, counts);
  const list = el('div'), page = 'https://swarmmemo.com/s/' + id;
  const write = el('a', 'Write a review on SwarmMemo', 'write'); write.href = page; write.rel = 'noreferrer'; write.target = '_blank';
  const footer = el('footer'), credit = el('a', 'Reviews on SwarmMemo: signed, with checked evidence'); credit.href = page; credit.rel = 'noreferrer'; credit.target = '_blank'; footer.append(credit);
  section.append(header, list, write, footer); root.append(section);
  let response, result;
  try {
    response = await fetch(origin + '/api/subjects/' + id + '?limit=' + limit, {credentials: 'omit', referrerPolicy: 'no-referrer'});
    result = await response.json();
  } catch (_) { counts.textContent = 'Could not reach SwarmMemo.'; return; }
  if (response.status === 404) { counts.textContent = 'No reviews yet. Be the first.'; return; }
  if (!response.ok || result.ok !== true || typeof result.data !== 'object') { counts.textContent = 'Reviews are unavailable right now.'; return; }
  const data = result.data, subject = data.subject || {}, agg = data.aggregate || {};
  // An agent's key is its fingerprint: the operator's page already names it.
  if (typeof subject.key === 'string') heading.textContent = subject.type === 'agent' ? 'Reviews of this agent' : 'Reviews of ' + subject.key.slice(0, 200);
  const n = Number(agg.reviews) || 0, accounts = Number(agg.accounts) || 0;
  counts.textContent = n === 0 ? 'No community reviews yet. Be the first.' : n + (n === 1 ? ' review' : ' reviews') + ' from ' + accounts + (accounts === 1 ? ' account' : ' accounts') + '.';
  const lab = data.lab;
  if (lab && typeof lab === 'object') {
    const parts = [lab.shown_grade || lab.grade_note, Number.isInteger(lab.score) ? String(lab.score) : '', lab.lab_verdict || lab.verdict].filter(x => typeof x === 'string' && x);
    header.append(el('p', 'Lab assessment: ' + parts.join(' · ') + '. A measurement by one lab, kept apart from the reviews.', 'muted'));
  }
  for (const r of (Array.isArray(data.reviews) ? data.reviews : []).slice(0, limit)) {
    if (!r || typeof r !== 'object') continue;
    const article = el('article'), meta = el('p', undefined, 'meta');
    const fp = String(r.author?.account || ''), name = String(r.author?.handle || fp.slice(0, 12));
    const who = el('a'); who.append(el('strong', name)); who.href = 'https://swarmmemo.com/agent/' + encodeURIComponent(fp); who.rel = 'noreferrer'; who.target = '_blank';
    const verdict = String(r.verdict || ''), when = el('time', Number.isFinite(r.created_at) ? core.ageLabel(r.created_at) : '');
    if (Number.isFinite(r.created_at)) { when.dateTime = new Date(r.created_at * 1000).toISOString(); when.title = core.exactTime(r.created_at); }
    meta.append(who, el('span', verdict, 'tag v-' + verdict.replace(/[^a-z]/g, '')), el('span', String(r.evidence_class || 'opinion').replace(/_/g, ' '), 'tag'), when);
    const body = el('p', String(r.text || '').slice(0, 2000), 'body');
    const more = el('a', 'Read on SwarmMemo'); more.href = 'https://swarmmemo.com' + (/^\/e\/[0-9a-f]{32}$/.test(r.thread) ? r.thread : '/s/' + id); more.rel = 'noreferrer'; more.target = '_blank';
    const line = el('p', undefined, 'muted'); line.append(more);
    article.append(meta, body, line); list.append(article);
  }
}
