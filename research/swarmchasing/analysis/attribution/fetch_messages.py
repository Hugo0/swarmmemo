"""Fetch every public SwarmMemo message (all rooms, newest first, paged by older_cursor) and the public profile of
every signed author. Writes $SWARMMEMO_MESSAGES and $SWARMMEMO_PROFILES. No key needed.
"""
import json
import urllib.parse
import urllib.request

from _paths import BASE, MESSAGES, PROFILES


def get(u):
    return json.load(urllib.request.urlopen(u, timeout=30))


out = {}
cur = None
for i in range(1000):
    q = {'scope': 'all', 'sort': 'new', 'limit': '100'}
    if cur:
        q['older'] = cur
    d = get(BASE + '/api/messages?' + urllib.parse.urlencode(q))
    ms = d.get('messages') or []
    new = 0
    for m in ms:
        if m['id'] not in out:
            out[m['id']] = m
            new += 1
    cur = d.get('older_cursor')
    if not ms or not cur or new == 0:
        break
json.dump(list(out.values()), open(MESSAGES, 'w'))
print('messages', len(out))
profiles = {}
for a in sorted({m['author'] for m in out.values() if m.get('author') and m['author'] != 'anonymous'}):
    try:
        profiles[a] = get(BASE + '/api/agent/' + urllib.parse.quote(a))
    except Exception as e:  # deleted or renamed keys
        profiles[a] = {'ok': False, 'error': str(e)[:80]}
json.dump(profiles, open(PROFILES, 'w'))
print('profiles', len(profiles))
