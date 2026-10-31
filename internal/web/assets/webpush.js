/* SwarmMemo browser notifications on Me, Settings (C667, web push).

   push.list says whether the server has web push on and gives its VAPID key;
   "Turn on" asks the browser for notification permission, registers /sw.js,
   subscribes with that key and signs push.subscribe with the browser's
   PushSubscription (endpoint, p256dh, auth). "Turn off" signs
   push.unsubscribe and drops the browser's subscription. The list shows every
   browser that gets notifications for this identity, by label and push
   service (never the endpoint); this browser's row is the one whose
   endpoint_sha256 matches. Every signed write goes through app.js
   (window.SwarmSign). Text is only set with textContent. */
(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const root = $('browser-notifications');
  if (!root) return;
  const {b64, unb64} = SwarmMemoCore;
  const node = (tag, className, text) => { const el = document.createElement(tag); if (className) el.className = className; if (text !== undefined) el.textContent = text; return el; };
  const say = (text, error = false) => { const el = $('push-status'); el.textContent = text; el.classList.toggle('error', error); };
  const stateLine = text => { $('push-state').textContent = text; };
  const toggle = $('push-toggle');
  const supported = 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window && window.isSecureContext;
  function guessLabel() {
    const ua = navigator.userAgent || '';
    const browser = /Edg\//.test(ua) ? 'Edge' : /Firefox\//.test(ua) ? 'Firefox' : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'Browser';
    const os = /iPhone|iPad/.test(ua) ? 'iOS' : /Android/.test(ua) ? 'Android' : /Windows/.test(ua) ? 'Windows' : /Mac OS X/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : '';
    return os ? `${browser} on ${os}` : browser;
  }
  async function sha256Hex(text) {
    const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(text));
    return [...new Uint8Array(digest)].map(b => b.toString(16).padStart(2, '0')).join('');
  }
  const sameKey = (sub, key) => { const k = sub?.options?.applicationServerKey; return !!k && b64(k) === key; };

  const ready = new Promise(resolve => { if (window.SwarmSign) resolve(window.SwarmSign); else document.addEventListener('swarmsign', () => resolve(window.SwarmSign), {once: true}); });
  ready.then(async S => {
    await S.ready;
    let info = null, mine = null, registration = null, epoch = 0;

    async function load() {
      const run = ++epoch;
      if (!S.identity) { toggle.hidden = true; $('push-list').hidden = true; return; }
      try { info = (await S.request({operation: 'push.list'}, true)).data; }
      catch (error) { if (run === epoch) say(error.message, true); return; }
      if (run !== epoch) return;
      let sub = null;
      if (supported && info.enabled) {
        try { registration = await navigator.serviceWorker.register('/sw.js'); sub = await registration.pushManager.getSubscription(); }
        catch (_) { registration = null; }
      }
      const hash = sub ? await sha256Hex(sub.endpoint) : '';
      mine = (info.subscriptions || []).find(r => r.state === 'active' && hash && r.endpoint_sha256 === hash && sameKey(sub, info.public_key)) || null;
      draw();
    }

    function draw() {
      if (!info.enabled) {
        stateLine(info.reason || 'Browser notifications are off on this server.');
        toggle.hidden = true;
      } else if (!supported) {
        stateLine(/iPhone|iPad/.test(navigator.userAgent || '') ? 'On iPhone and iPad, add SwarmMemo to your Home Screen first (Share, Add to Home Screen), then turn notifications on from there.' : 'This browser cannot show web push notifications.');
        toggle.hidden = true;
      } else if (Notification.permission === 'denied') {
        stateLine('Notifications are blocked for this site in your browser settings. Allow them there, then come back.');
        toggle.hidden = true;
      } else {
        stateLine(mine ? 'On in this browser.' : 'Off in this browser.');
        toggle.textContent = mine ? 'Turn off in this browser' : 'Turn on notifications';
        toggle.hidden = false;
      }
      const rows = info.subscriptions || [];
      const list = $('push-list');
      list.hidden = !rows.length;
      list.replaceChildren(...rows.map(r => {
        const row = node('li'), what = node('span');
        const name = r.label || r.service || 'A browser';
        what.append(node('strong', '', name), node('span', 'small muted', ' · ' + (mine && r.subscription_id === mine.subscription_id ? 'this browser' : r.service) + (r.state === 'gone' ? ' · stopped: the browser unsubscribed' : '')));
        row.append(what);
        const remove = node('button', 'quiet-button danger', 'Remove'); remove.type = 'button';
        remove.setAttribute('aria-label', 'Stop notifications to ' + name);
        remove.addEventListener('click', () => act(remove, async () => {
          await S.request({operation: 'push.unsubscribe', target: r.subscription_id, request_id: S.uuid()}, true);
          if (mine && r.subscription_id === mine.subscription_id) await dropLocal();
          say('Removed. ' + name + ' gets no more notifications.');
        }));
        row.append(remove);
        return row;
      }));
    }

    async function dropLocal() {
      try { const sub = await registration?.pushManager.getSubscription(); if (sub) await sub.unsubscribe(); } catch (_) { /* the server row is gone either way */ }
    }

    async function act(button, step) {
      button.disabled = true; say('');
      try { await step(); }
      catch (error) { say(error.message || 'That did not work. Try again.', true); }
      finally { button.disabled = false; void load(); }
    }

    async function turnOn() {
      const permission = await Notification.requestPermission();
      if (permission !== 'granted') throw Error('Notifications were not allowed. Allow them for this site in your browser to turn this on.');
      registration = registration || await navigator.serviceWorker.register('/sw.js');
      await navigator.serviceWorker.ready;
      let sub = await registration.pushManager.getSubscription();
      if (sub && !sameKey(sub, info.public_key)) { await sub.unsubscribe(); sub = null; }
      if (!sub) sub = await registration.pushManager.subscribe({userVisibleOnly: true, applicationServerKey: unb64(info.public_key)});
      const data = {schema: 1, endpoint: sub.endpoint, p256dh: b64(sub.getKey('p256dh')), auth: b64(sub.getKey('auth')), label: guessLabel()};
      await S.request({operation: 'push.subscribe', data: JSON.stringify(data), request_id: S.uuid()}, true);
      say('On. This browser shows a notification when your inbox gets news: counts, never message text.');
    }

    toggle.addEventListener('click', () => act(toggle, async () => {
      if (mine) {
        await S.request({operation: 'push.unsubscribe', target: mine.subscription_id, request_id: S.uuid()}, true);
        await dropLocal();
        say('Off in this browser.');
      } else await turnOn();
    }));
    // Read only once the section is on screen: push.list is a signed read.
    if ('IntersectionObserver' in window) {
      const seen = new IntersectionObserver(entries => { if (entries.some(e => e.isIntersecting)) { seen.disconnect(); void load(); } });
      seen.observe(root);
    } else void load();
  });
})();
