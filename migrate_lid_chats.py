"""One-off: merge DM chats stored under xxx@lid into the phone-number chat (xxx@s.whatsapp.net).
Run with the bridge STOPPED. Uses whatsmeow's own LID map (whatsapp.db)."""
import os, sqlite3

base = os.environ.get('WHATSAPP_STORE_DIR') or os.path.join(os.environ['LOCALAPPDATA'], 'whatsapp-mcp', 'store')
lidmap = dict(sqlite3.connect(os.path.join(base, 'whatsapp.db')).execute('select lid, pn from whatsmeow_lid_map'))
db = sqlite3.connect(os.path.join(base, 'messages.db'))

moved_chats = moved_msgs = 0
for jid, name, last in db.execute("select jid, name, last_message_time from chats where jid like '%@lid'").fetchall():
    pn = lidmap.get(jid.split('@')[0])
    if not pn:
        continue
    pn_jid = f'{pn}@s.whatsapp.net'
    existing = db.execute('select name, last_message_time from chats where jid=?', (pn_jid,)).fetchone()
    if existing:
        # keep the real contact name (LID chats are named after the LID number) and the newest timestamp
        new_name = existing[0] if existing[0] and existing[0] != pn else name
        db.execute('update chats set name=?, last_message_time=max(coalesce(last_message_time, ""), ?) where jid=?',
                   (new_name, last or '', pn_jid))
    else:
        db.execute('insert into chats (jid, name, last_message_time) values (?, ?, ?)',
                   (pn_jid, pn if name == jid.split('@')[0] else name, last))
    # INSERT OR IGNORE semantics: drop LID duplicates of messages already stored under the phone JID
    db.execute('delete from messages where chat_jid=? and id in (select id from messages where chat_jid=?)', (jid, pn_jid))
    moved_msgs += db.execute('update messages set chat_jid=? where chat_jid=?', (pn_jid, jid)).rowcount
    db.execute('delete from chats where jid=?', (jid,))
    moved_chats += 1

# Senders stored as bare LID user parts (or user@lid) -> phone number, so names resolve
fixed_senders = 0
for lid, pn in lidmap.items():
    fixed_senders += db.execute('update messages set sender=? where sender in (?, ?)', (pn, lid, f'{lid}@lid')).rowcount

db.commit()
print(f'remetentes LID convertidos para número: {fixed_senders}')
left =db.execute("select count(*) from chats where jid like '%@lid'").fetchone()[0]
print(f'chats mesclados: {moved_chats}, mensagens movidas: {moved_msgs}, chats LID sem correspondência: {left}')
