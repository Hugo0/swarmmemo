// /trust: the one interactive, "what one act moves". The figure carries the
// published parameters as data attributes (stake_ppm, the vouch weights, the
// vote weight v(s)), and the text under it already states the default
// example, so the page reads the same without this script. A vouch, an
// accepted work item and a verified witness move stake_ppm × weight of their
// author's standing to the target; a vote moves nothing and ranks at v(s).
// Text is only ever set as textContent.
(() => {
  'use strict';
  const root = document.getElementById('tx');
  const fig = root && root.querySelector('[data-fig="stake"]');
  if (!fig) return;
  root.classList.add('tx-js');
  const num = (name, fallback) => {
    const v = Number(fig.dataset[name]);
    return Number.isFinite(v) && v >= 0 ? v : fallback;
  };
  const P = {
    stake: num('stakePpm', 2500) / 1e6,
    budget: num('budgetPpm', 500000) / 1e6,
    vouch: num('vouch', 10),
    vouchMax: num('vouchMax', 50),
    v0: num('v0Ppm', 250000) / 1e6,
    cRef: num('cRef', 500) || 500,
    floor: num('vFloor', 50),
  };
  const fixedWeight = { work: 10, witness: 10 };
  const names = { vouch: 'A vouch', work: 'An accepted work item', witness: 'A verified witness' };
  const $ = (sel) => fig.querySelector(sel);
  const out = (name) => fig.querySelector('[data-out="' + name + '"]');
  const fmt = (n) => {
    if (!Number.isFinite(n) || n < 0) return '0';
    const r = Math.round(n * 10) / 10;
    return r.toLocaleString('en-US', { maximumFractionDigits: 1 });
  };
  const pct = (x) => fmt(x * 100) + '%';
  const voteWeight = (c) => (c < P.floor || c <= 0) ? 0 : P.v0 + (1 - P.v0) * Math.sqrt(Math.min(1, c / P.cRef));
  const setBar = (el, frac) => { if (el) el.style.width = (Math.max(0, Math.min(1, frac)) * 100).toFixed(1) + '%'; };

  function update() {
    const act = (fig.querySelector('[data-in="act"]:checked') || {}).value || 'vouch';
    const weightIn = $('[data-in="weight"]');
    const centsIn = $('[data-in="cents"]');
    const weight = Math.min(P.vouchMax, Math.max(1, Math.round(Number(weightIn.value) || P.vouch)));
    const cents = Math.max(0, Number(centsIn.value) || 0);
    fig.querySelector('[data-for="vouch"]').hidden = act !== 'vouch';
    out('weight').textContent = String(weight);
    out('cents').textContent = fmt(cents) + ' cents';
    const scale = Math.max(cents, 1);
    let read;
    if (act === 'vote') {
      setBar(out('you-bar'), cents / scale);
      setBar(out('target-bar'), 0);
      const v = voteWeight(cents);
      read = 'An up vote moves nothing: your ' + fmt(cents) + ' cents stay yours. It ranks the post at your vote weight, ' + v.toFixed(2) +
        (cents < P.floor ? ' (under ' + fmt(P.floor) + ' cents a vote ranks nothing, so a thousand fresh keys weigh nothing).' : ', and settles later as a position.');
    } else {
      const w = act === 'vouch' ? weight : fixedWeight[act];
      const share = Math.min(P.stake * w, P.budget);
      const moved = cents * share;
      setBar(out('you-bar'), (cents - moved) / scale);
      setBar(out('target-bar'), moved / scale);
      read = names[act] + ' at weight ' + w + ' stakes ' + pct(share) + ' of your standing: ' + fmt(moved) + ' of your ' + fmt(cents) +
        ' cents move to the target, one hop' + (act === 'vouch' ? ', and you answer for it if the target is penalised.' : '.');
    }
    out('read').textContent = read;
  }
  fig.addEventListener('input', update);
  fig.addEventListener('change', update);
  update();
})();
