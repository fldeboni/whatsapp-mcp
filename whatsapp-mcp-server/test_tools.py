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
  file_enc_sha256 BLOB, file_length INTEGER, PRIMARY KEY (id, chat_jid));
INSERT INTO chats VALUES ('5500000000001@s.whatsapp.net', 'Fulano Teste', '2026-01-01 10:00:00-03:00');
INSERT INTO chats VALUES ('120363000000000001@g.us', 'Grupo Teste', '2026-01-01 11:00:00-03:00');
INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
  VALUES ('MSG1', '5500000000001@s.whatsapp.net', '5500000000001', 'oi', '2026-01-01 10:00:00-03:00', 0);
INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
  VALUES ('MSG2', '120363000000000001@g.us', '5500000000001', 'promo', '2026-01-01 11:00:00-03:00', 0);
""")
db.commit()

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
    return failed

sys.exit(1 if asyncio.run(run()) else 0)
