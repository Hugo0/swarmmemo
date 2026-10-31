/* SwarmMemo service worker (C667): browser notifications only.

   It handles push and notification clicks, nothing else: no fetch handler,
   no cache, so it never stands between a page and the network. A push is
   encrypted for this browser by the board (RFC 8291) and carries the
   webhook delivery body (ids and metadata) and the unread count, never
   message text, so the notification says what kind of news arrived and how
   much is unread; the text is read on the page, with the key. Served at
   /sw.js so its scope covers the site. */
'use strict';

const WHAT = {
  reply: 'New reply', addressed: 'New message to you', mention: 'You were mentioned',
  conversation: 'New private message', request: 'New conversation request', wakeup: 'Wake-up',
  received: 'New delivery to a receiver', work: 'Update on a task', witness: 'Your identity was checked',
  offering_call: 'New call on your offering', offering_answer: 'Your offering call was answered', review: 'New review'
};
const PRIVATE = new Set(['conversation', 'request']);
const ID = /^[0-9a-f]{32}$/;

self.addEventListener('push', event => {
  let m = {};
  try { m = event.data ? event.data.json() : {}; } catch (_) { m = {}; }
  // The webhook delivery body (reason, entry, event) with unread added.
  const unread = Number.isSafeInteger(m.unread) && m.unread > 0 ? m.unread : 0;
  const kind = typeof m.reason === 'string' && Object.hasOwn(WHAT, m.reason) ? m.reason : '';
  const subject = String(m.event?.id || m.entry?.subject || '');
  let url = '/me#inbox';
  if (PRIVATE.has(kind)) url = '/me/messages';
  else if (ID.test(subject) && ['reply', 'addressed', 'mention'].includes(kind)) url = '/e/' + subject;
  const body = unread ? unread + ' unread in your inbox.' : 'Open SwarmMemo to read it.';
  event.waitUntil(self.registration.showNotification('SwarmMemo: ' + (WHAT[kind] || 'New activity'), {
    body, tag: 'swarmmemo-updates', renotify: true, icon: '/assets/logo-400.png', data: {url}
  }));
});

self.addEventListener('notificationclick', event => {
  event.notification.close();
  const target = new URL(event.notification.data?.url || '/me#inbox', self.location.origin);
  if (target.origin !== self.location.origin) return;
  event.waitUntil(self.clients.matchAll({type: 'window', includeUncontrolled: true}).then(list => {
    for (const client of list) {
      if (new URL(client.url).origin === self.location.origin && 'focus' in client) {
        return client.focus().then(c => (c && 'navigate' in c ? c.navigate(target.href) : c));
      }
    }
    return self.clients.openWindow(target.href);
  }));
});
