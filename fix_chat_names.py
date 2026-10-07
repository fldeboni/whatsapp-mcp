"""Gives a real name to direct chats stored with just the phone number / LID as name
(the address book syncs after the first messages, so early chats got the number).
Safe to run with the bridge running; only touches chats whose name is purely digits.
Run: <venv python> fix_chat_names.py"""
import os, sqlite3

base = os.environ.get('WHATSAPP_STORE_DIR') or os.path.join(os.path.expanduser('~'), '.whatsapp-mcp', 'store')
book = sqlite3.connect(f"file:{os.path.join(base, 'whatsapp.db')}?mode=ro", uri=True)
pn_to_lid = {pn: lid for lid, pn in book.execute('SELECT lid, pn FROM whatsmeow_lid_map')}
names = {}
for jid, name in book.execute("""SELECT their_jid, COALESCE(NULLIF(full_name,''), NULLIF(first_name,''),
                                        NULLIF(business_name,''), NULLIF(push_name,'')) FROM whatsmeow_contacts"""):
    if name:
        names[jid.split('@')[0].split(':')[0]] = name

db = sqlite3.connect(os.path.join(base, 'messages.db'), timeout=10)
fixed = 0
for jid, name in db.execute("SELECT jid, name FROM chats WHERE jid NOT LIKE '%@g.us'").fetchall():
    if name and not name.isdigit():
        continue
    user = jid.split('@')[0]
    new = names.get(user) or names.get(pn_to_lid.get(user, ''))
    if new:
        db.execute('UPDATE chats SET name = ? WHERE jid = ?', (new, jid))
        fixed += 1
db.commit()
left = db.execute("SELECT COUNT(*) FROM chats WHERE jid NOT LIKE '%@g.us' AND (name IS NULL OR name GLOB '[0-9]*')").fetchone()[0]
print(f'conversas renomeadas: {fixed} | ainda só com número (sem nome na agenda): {left}')
