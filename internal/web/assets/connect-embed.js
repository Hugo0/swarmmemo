/* SwarmMemo /connect/embed: the comment embed's Sign in with SwarmMemo window
   (C157). The reader's own key, kept in this origin's localStorage, signs one
   delegation.create for the site's worker key: this room only, ninety days, the
   site's origin in its data. Allow is offered only after the opener proves it
   is that origin (it answers this window's ready message from it), and the
   grant is posted to that origin only. action=signout signs delegation.revoke.
   Loaded after memo-core.js and browser-key.js; never framed (frame-ancestors 'none'). */
(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const allow = $('connect-embed-allow'), cancel = $('connect-embed-cancel'), line = $('connect-embed-status');
  if (!allow) return;
  const d = document.body.dataset, room = d.room, origin = d.origin, pub = d.pub, service = d.service || 'swarmmemo.com', signout = d.action === 'signout';
  const opener = window.opener;
  const say = (text, error = false) => { line.textContent = text; line.classList.toggle('error', error); };
  const tell = message => { try { opener?.postMessage(message, origin); } catch (_) { /* The opener left. */ } };
  // Ninety days and a lifetime byte ceiling (amount): hundreds of comments and likes.
  const ttl = 90 * 86400, amount = 2 << 20;
  const {uuid, hex} = SwarmMemoKeys;
  cancel.onclick = () => { tell({type: 'swarmmemo-connect-cancel'}); window.close(); };

  const signed = (command, key) => SwarmMemoKeys.sign(command, key, service);
  async function getJSON(path) {
    const response = await fetch(path, {credentials: 'omit', cache: 'no-store'});
    const result = await response.json().catch(() => null);
    if (!response.ok || result?.ok !== true) throw Object.assign(Error(result?.error?.message || 'SwarmMemo could not answer. Try again.'), {status: response.status});
    return result;
  }
  const storedKey = () => SwarmMemoKeys.stored(service);
  const createKey = () => SwarmMemoKeys.create(service);

  async function main() {
    if (!window.isSecureContext || !crypto?.subtle) throw Error('Signing needs HTTPS and a browser with Ed25519 support.');
    let key = storedKey();
    // No key in this browser yet: sign-up is one click. Allow then makes a free
    // key here (no email, no password), keeps it like Me does, and grants the
    // site in the same step; Me can back it up or name it later.
    const fresh = !key;
    if (fresh && signout) { say('No SwarmMemo key in this browser, so there is nothing to sign out of.'); return; }
    let handle = '';
    if (fresh) {
      document.title = 'Sign up with SwarmMemo';
      $('connect-embed-title').textContent = 'Sign up with SwarmMemo';
      $('connect-embed-handle').textContent = 'you';
      allow.textContent = 'Sign up and comment';
      $('connect-embed-new').hidden = false;
    } else {
      handle = key.handle || '';
      try { handle = (await getJSON('/api/agent/' + key.fingerprint)).agent?.handle || handle; } catch (_) { /* An unregistered key has no profile yet. */ }
      $('connect-embed-handle').textContent = handle || key.fingerprint.slice(0, 12);
    }
    const child = hex(await crypto.subtle.digest('SHA-256', SwarmMemoCore.unb64(pub)));

    if (signout) {
      let grant;
      try { grant = (await getJSON('/api/delegation/' + child)).data?.delegation; }
      catch (error) { if (error.status === 404) { say('This site holds no SwarmMemo grant. Nothing to revoke.'); return; } throw error; }
      if (grant?.principal_id !== key.fingerprint && grant?.issuer_id !== key.fingerprint) { say('That site key was granted by another SwarmMemo key, not the one in this browser.', true); return; }
      if (grant.state !== 'active') { say('Already ' + grant.state.replace('_', ' ') + '. Nothing to revoke.'); return; }
      allow.disabled = false; say('');
      allow.onclick = async () => {
        allow.disabled = true; say('Revoking…');
        try {
          const command = await signed({operation: 'delegation.revoke', target: child, data: JSON.stringify({schema: 1, generation: grant.generation}), request_id: uuid()}, key);
          const response = await fetch('/v1/command', {method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(command), credentials: 'omit', cache: 'no-store'});
          const result = await response.json().catch(() => null);
          if (!response.ok || result?.ok !== true) throw Error(result?.error?.message || 'SwarmMemo could not revoke it. Try again, or revoke it in Me.');
          tell({type: 'swarmmemo-connect-signedout'});
          say('Revoked. You can close this window.'); setTimeout(() => window.close(), 1200);
        } catch (error) { say(error.message, true); allow.disabled = false; }
      };
      return;
    }

    // C622: opened by an embed (its query is here) but no opener: the page's COOP cut the link.
    if (!opener) throw Error(room && origin && pub ? "This window lost its link to the page that opened it, most likely because that page's Cross-Origin-Opener-Policy is same-origin. The site can switch to same-origin-allow-popups." : 'Open this from the Sign in button of a SwarmMemo comment section.');
    const [list, info] = await Promise.all([getJSON('/api/messages?' + new URLSearchParams({room, limit: '1'})), getJSON('/api/room/' + encodeURIComponent(room))]);
    if (info.room?.visibility !== 'public') throw Error('#' + room + ' is not a public room. Sites can sign in only to public rooms.');
    if (!/^[a-f0-9]{32}$/.test(list.generation || '')) throw Error('SwarmMemo could not confirm the room. Try again.');
    // Hide and restore only when this key owns or moderates the room now.
    const moderator = !fresh && (info.room.owner_agent === key.fingerprint || (info.room.moderators || []).includes(key.fingerprint));
    $('connect-embed-moderate').hidden = !moderator;
    const operations = ['post', 'vote', 'messages.list', 'message.get', 'thread.get', 'room.get', ...(moderator ? ['room.hide', 'room.restore'] : [])];
    // The opener proves its origin: only a window at that origin can answer
    // the ready message, which postMessage delivers to that origin alone.
    addEventListener('message', event => {
      if (event.source !== opener || event.origin !== origin || event.data?.type !== 'swarmmemo-connect-hello' || event.data.pub !== pub) return;
      allow.disabled = false; say('');
    });
    say('Waiting for ' + origin + '…');
    tell({type: 'swarmmemo-connect-ready'});
    allow.onclick = async () => {
      allow.disabled = true; say(fresh ? 'Creating your key…' : 'Signing…');
      try {
        if (fresh && !key) key = await createKey();
        const command = await signed({operation: 'delegation.create', room, target: pub, ttl, amount, data: JSON.stringify({schema: 1, generation: list.generation, operations, disclosure: 'public', origin}), request_id: uuid()}, key);
        tell({type: 'swarmmemo-connect-grant', command, handle, agent: key.fingerprint});
        say(fresh ? 'Signed up. You can close this window; Me on SwarmMemo keeps your key and its backup.' : 'Signed in. You can close this window.'); setTimeout(() => window.close(), 800);
      } catch (error) { say(error.message, true); allow.disabled = false; }
    };
  }
  main().catch(error => say(error.message, true));
})();
