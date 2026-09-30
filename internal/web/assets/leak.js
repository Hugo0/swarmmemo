/* SwarmMemo leak patterns in the browser (RFC 0013 §5.3): the composer's
 * local check, before anything is signed or sent. It runs the one shared list,
 * /assets/leak-patterns.json (generated from internal/leakscan), with the same
 * rules as the Go scanner: a rule's group, when set, is the span; card numbers
 * need 13 to 19 digits passing Luhn; IBANs need the mod-97 check. An
 * isomorphic ES module; importing it does no I/O. */
export const MAX_FINDINGS = 256;

function digits(s) { return s.replace(/[^0-9]/g, ''); }
export function luhn(s) {
  const d = digits(s);
  if (d.length < 13 || d.length > 19) return false;
  let sum = 0;
  for (let i = 0; i < d.length; i++) { let v = Number(d[d.length - 1 - i]); if (i % 2) { v *= 2; if (v > 9) v -= 9; } sum += v; }
  return sum % 10 === 0;
}
export function mod97(s) {
  s = s.replace(/ /g, '');
  if (s.length < 15 || s.length > 34) return false;
  let rem = 0;
  for (const c of s.slice(4) + s.slice(0, 4)) {
    if (c >= '0' && c <= '9') rem = (rem * 10 + Number(c)) % 97;
    else if (c >= 'A' && c <= 'Z') rem = (rem * 100 + c.charCodeAt(0) - 55) % 97;
    else return false;
  }
  return rem === 1;
}
/** The rules of a leak-patterns.json object, compiled; a rule this engine
 * cannot compile is left out, never guessed at. */
export function compile(list) {
  if (list?.schema !== 1 || !Array.isArray(list.rules)) throw Error('not a leak pattern list');
  return list.rules.flatMap(rule => { try { return [{...rule, re: new RegExp(rule.pattern, 'dg')}]; } catch (_) { return []; } });
}
/** Findings {rule, category, note, start, end} by offset (UTF-16 units). */
export function scan(text, rules) {
  const out = [];
  for (const rule of rules) {
    for (const match of text.matchAll(rule.re)) {
      const [start, end] = rule.group ? match.indices[rule.group] || [-1, -1] : [match.index, match.index + match[0].length];
      if (start < 0 || start === end) continue;
      const value = text.slice(start, end);
      if ((rule.luhn && !luhn(value)) || (rule.mod97 && !mod97(value))) continue;
      out.push({rule: rule.id, category: rule.category, note: rule.note || '', start, end});
    }
  }
  out.sort((a, b) => a.start - b.start || b.end - a.end);
  return out.slice(0, MAX_FINDINGS);
}
/** What a finding of category does to a message about to be sent: the
 * agent's own overrides (outbound.actions), then the list's actions table;
 * hold for a category neither names (leakscan.Action). */
export function action(category, actions, overrides) {
  return [overrides?.[category], actions?.[category]].find(a => a === 'hold' || a === 'warn') || 'hold';
}
/** hold when a finding, or a classifier score at or above threshold, holds;
 * warn when every one only warns; pass when there are none
 * (leakscan.Verdict). */
export function verdict(findings, actions, overrides, scores = {}, threshold = 1) {
  let out = 'pass';
  const found = category => { if (action(category, actions, overrides) === 'hold') out = 'hold'; else if (out === 'pass') out = 'warn'; };
  for (const f of findings) found(f.category);
  for (const [category, score] of Object.entries(scores || {})) if (score >= threshold) found(category);
  return out;
}
/** Each finding's span as «REDACTED:rule»; overlapping findings become one
 * span, labelled with the first. */
export function redact(text, findings) {
  const spans = findings.filter(f => f.start >= 0 && f.start < f.end && f.end <= text.length).sort((a, b) => a.start - b.start);
  let out = '', at = 0;
  for (let i = 0; i < spans.length;) {
    const {rule, start} = spans[i]; let end = spans[i].end;
    for (i++; i < spans.length && spans[i].start < end; i++) end = Math.max(end, spans[i].end);
    out += text.slice(at, start) + '«REDACTED:' + rule + '»'; at = end;
  }
  return out + text.slice(at);
}
