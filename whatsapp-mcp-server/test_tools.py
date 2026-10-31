"""Calls every read-only tool through FastMCP (same validation path as Claude) against a throwaway DB.
Run: python test_tools.py   (exit code 1 on failure). No WhatsApp/bridge needed."""
import asyncio, os, sqlite3, sys, tempfile

store = tempfile.mkdtemp()
os.environ['WHATSAPP_STORE_DIR'] = store
db = sqlite3.connect(os.path.join(store, 'messages.db'))
db.executescript("""
CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, sender_jid TEXT, content TEXT, timestamp TIMESTAMP,
  is_from_me BOOLEAN, media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB,
  file_enc_sha256 BLOB, file_length INTEGER, original_content TEXT, edited_at TIMESTAMP, deleted_at TIMESTAMP,
  PRIMARY KEY (id, chat_jid));
INSERT INTO chats VALUES ('5500000000001@s.whatsapp.net', 'Fulano Teste', '2026-01-01 10:00:00-03:00');
INSERT INTO chats VALUES ('120363000000000001@g.us', 'Grupo Teste', '2026-01-01 11:00:00-03:00');
INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
  VALUES ('MSG1', '5500000000001@s.whatsapp.net', '5500000000001', 'oi', '2026-01-01 10:00:00-03:00', 0);
INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
  VALUES ('MSG2', '120363000000000001@g.us', '5500000000001', 'promo', '2026-01-01 11:00:00-03:00', 0);
""")
db.executescript("""
INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, deleted_at)
  VALUES ('MSGDEL', '5500000000001@s.whatsapp.net', '5500000000001', 'mensagem apagada', '2026-01-01 10:01:00-03:00', 0, '2026-01-01 10:02:00-03:00');
INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, edited_at, original_content)
  VALUES ('MSGEDT', '5500000000001@s.whatsapp.net', '5500000000001', 'texto novo', '2026-01-01 10:03:00-03:00', 0, '2026-01-01 10:04:00-03:00', 'texto velho');
""")
db.executescript("""
CREATE TABLE message_receipts (message_id TEXT, chat_jid TEXT, recipient TEXT, delivered_at TIMESTAMP, read_at TIMESTAMP,
  played_at TIMESTAMP, PRIMARY KEY (message_id, chat_jid, recipient));
INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
  VALUES ('MSGME', '5500000000001@s.whatsapp.net', 'me', 'minha mensagem', '2026-01-01 10:05:00-03:00', 1);
INSERT INTO message_receipts VALUES ('MSGME', '5500000000001@s.whatsapp.net', '5500000000001', '2026-01-01 10:05:01-03:00', '2026-01-01 10:06:00-03:00', NULL);
""")
db.executescript("""
CREATE TABLE message_reactions (message_id TEXT, chat_jid TEXT, reactor TEXT, from_me BOOLEAN, emoji TEXT, reacted_at TIMESTAMP,
  PRIMARY KEY (message_id, chat_jid, reactor));
INSERT INTO message_reactions VALUES ('MSG1', '5500000000001@s.whatsapp.net', '5500000000001', 0, '👍', '2026-01-01 10:00:30-03:00');
INSERT INTO message_reactions VALUES ('MSG1', '5500000000001@s.whatsapp.net', '5599999999999', 1, '❤️', '2026-01-01 10:00:40-03:00');
""")
db.commit()

# Address book (whatsmeow session store): a contact with no synced chat, stored under a LID with a device suffix
book = sqlite3.connect(os.path.join(store, 'whatsapp.db'))
book.executescript("""
CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT, push_name TEXT, business_name TEXT);
CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT);
INSERT INTO whatsmeow_contacts VALUES ('me', '9990001112223:38@lid', NULL, 'Lívia Agenda', NULL, NULL);
INSERT INTO whatsmeow_lid_map VALUES ('9990001112223', '5500000000009');
""")
book.commit()

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import main  # noqa: E402  (must come after WHATSAPP_STORE_DIR is set)

CALLS = [
    ('search_contacts', {'query': 'fulano'}),
    ('list_chats', {'limit': 5}),
    ('list_chats', {'limit': 5, 'include_last_message': False}),
    ('list_messages', {'limit': 5}),
    ('get_chat', {'chat_jid': '5500000000001@s.whatsapp.net'}),
    ('get_direct_chat_by_contact', {'sender_phone_number': '5500000000001'}),
    ('get_contact_chats', {'jid': '5500000000001'}),
    ('get_last_interaction', {'jid': '5500000000001'}),
    ('get_message_context', {'message_id': 'MSG1'}),
]

async def run():
    failed = 0
    for name, args in CALLS:
        try:
            await main.mcp.call_tool(name, args)
            print(f'ok   {name}')
        except Exception as e:
            failed += 1
            print(f'FAIL {name}: {str(e)[:200]}')
    # Address-book search: no accent in the query, LID + device suffix resolved to the phone JID
    res = await main.mcp.call_tool('search_contacts', {'query': 'livia agenda'})
    text = ' '.join(getattr(b, 'text', '') for b in (res[0] if isinstance(res, tuple) else res))
    if '5500000000009@s.whatsapp.net' in text:
        print('ok   search_contacts (agenda, sem acento, LID->numero)')
    else:
        failed += 1
        print(f'FAIL search_contacts agenda: {text[:200]}')
    # Deleted/edited messages keep their content and show it with a mark
    res = await main.mcp.call_tool('list_messages', {'chat_jid': '5500000000001@s.whatsapp.net', 'limit': 10, 'include_context': False})
    text = ' '.join(getattr(b, 'text', '') for b in (res[0] if isinstance(res, tuple) else res))
    if '[APAGADA PELO REMETENTE' in text and 'mensagem apagada' in text and 'original: "texto velho"' in text and 'texto novo' in text:
        print('ok   list_messages (apagada/editada com conteudo original)')
    else:
        failed += 1
        print(f'FAIL marcas apagada/editada: {text[:300]}')
    # Receipts of my own messages: mark in list_messages + per-recipient tool
    res = await main.mcp.call_tool('list_messages', {'chat_jid': '5500000000001@s.whatsapp.net', 'limit': 10, 'include_context': False})
    text = ' '.join(getattr(b, 'text', '') for b in (res[0] if isinstance(res, tuple) else res))
    res2 = await main.mcp.call_tool('get_message_status', {'chat_jid': '5500000000001@s.whatsapp.net', 'message_id': 'MSGME'})
    text2 = ' '.join(getattr(b, 'text', '') for b in (res2[0] if isinstance(res2, tuple) else res2))
    if '[LIDA em 2026-01-01 10:06' in text and '2026-01-01 10:06:00' in text2:
        print('ok   recibos (lida/entregue)')
    else:
        failed += 1
        print(f'FAIL recibos: {text[:200]} | {text2[:200]}')
    # Reactions: who reacted with what, on any message
    if '[REAÇÕES: 👍 Fulano Teste, ❤️ Eu]' in text:
        print('ok   reacoes')
    else:
        failed += 1
        print(f'FAIL reacoes: {text[:300]}')
    return failed

sys.exit(1 if asyncio.run(run()) else 0)
