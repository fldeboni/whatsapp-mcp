import sqlite3
from datetime import datetime
from dataclasses import dataclass
from typing import Any, Dict, Optional, List, Tuple
import os.path
import requests
import json
import audio

# Same location the bridge uses: WHATSAPP_STORE_DIR or %USERPROFILE%\.whatsapp-mcp\store
# (not AppData: Windows virtualizes it for packaged apps like Claude desktop; not OneDrive: it syncs)
STORE_DIR = os.environ.get('WHATSAPP_STORE_DIR') or os.path.join(
    os.path.expanduser('~'), '.whatsapp-mcp', 'store')
MESSAGES_DB_PATH = os.path.join(STORE_DIR, 'messages.db')
WHATSAPP_DB_PATH = os.path.join(STORE_DIR, 'whatsapp.db')  # whatsmeow session store: address book + LID map (read-only use)


def _fold(text) -> str:
    """Lowercase without accents, so 'livia' matches 'Lívia' and 'joao' matches 'João'."""
    import unicodedata
    if text is None:
        return ''
    return ''.join(c for c in unicodedata.normalize('NFKD', str(text)) if not unicodedata.combining(c)).lower()


def _register_fold(conn) -> None:
    conn.create_function('fold', 1, _fold, deterministic=True)
WHATSAPP_API_BASE_URL = "http://localhost:8080/api"

@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None

@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")

@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: str

@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]

def get_sender_name(sender_jid: str) -> str:
    """Display name for a sender (bare number, LID or JID). Only DIRECT chats and the address book are used:
    the old "jid LIKE %number%" also matched groups created by that person (their ID starts with the creator's
    number), so messages showed up as sent by a group name."""
    user = sender_jid.split('@')[0].split(':')[0]
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        try:
            row = conn.execute("SELECT name FROM chats WHERE jid IN (?, ?, ?) AND name IS NOT NULL AND name != '' LIMIT 1",
                               (sender_jid, f'{user}@s.whatsapp.net', f'{user}@lid')).fetchone()
        finally:
            conn.close()
        if row and not row[0].isdigit():
            return row[0]
    except sqlite3.Error as e:
        print(f"Database error while getting sender name: {e}")
    try:
        book = sqlite3.connect(f'file:{WHATSAPP_DB_PATH}?mode=ro', uri=True)
        try:
            users = [user]
            pn = book.execute('SELECT pn FROM whatsmeow_lid_map WHERE lid = ?', (user,)).fetchone()
            lid = book.execute('SELECT lid FROM whatsmeow_lid_map WHERE pn = ?', (user,)).fetchone()
            # phone-number entry first: it carries the name saved in the address book (LID entries only the profile name)
            users = ([pn[0]] if pn else []) + users + ([lid[0]] if lid else [])
            for u in users:
                row = book.execute("""SELECT COALESCE(NULLIF(full_name,''), NULLIF(first_name,''), NULLIF(business_name,''),
                                             NULLIF(push_name,'')) FROM whatsmeow_contacts
                                      WHERE their_jid LIKE ? OR their_jid LIKE ? LIMIT 1""", (f'{u}@%', f'{u}:%')).fetchone()
                if row and row[0]:
                    return row[0]
        finally:
            book.close()
    except sqlite3.Error as e:
        print(f"Address book error while getting sender name: {e}")
    return sender_jid

def _edit_delete_marks(message_id: str, chat_jid: str) -> str:
    """Flags kept by the bridge: a deleted message keeps its content (so you know what was said AND that it was
    deleted); an edited one keeps its first text in original_content."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        try:
            try:
                row = conn.execute('SELECT deleted_at, edited_at, original_content, deleted_for_me_at, starred_at, pinned_until FROM messages '
                                   'WHERE id = ? AND chat_jid = ?', (message_id, chat_jid)).fetchone()
            except sqlite3.Error:  # bridge not yet restarted with the deleted_for_me_at column
                row = conn.execute('SELECT deleted_at, edited_at, original_content, NULL, NULL, NULL FROM messages '
                                   'WHERE id = ? AND chat_jid = ?', (message_id, chat_jid)).fetchone()
        finally:
            conn.close()
    except sqlite3.Error:
        return ''  # old database without the columns
    if not row:
        return ''
    deleted_at, edited_at, original, deleted_for_me_at, starred_at, pinned_until = row
    marks = '[FAVORITA] ' if starred_at else ''
    if pinned_until:
        try:
            until = datetime.fromisoformat(str(pinned_until))
            if until > datetime.now(until.tzinfo):
                marks += f'[FIXADA até {until.astimezone().strftime("%d/%m %H:%M")}] '
        except ValueError:
            pass
    if deleted_at:
        marks += f'[APAGADA PELO REMETENTE em {str(deleted_at)[:16]}] '
    if deleted_for_me_at:
        marks += f'[APAGADA SÓ PARA MIM em {str(deleted_for_me_at)[:16]}] '
    if edited_at:
        orig = (original or '').replace('\n', ' / ')
        marks += f'[EDITADA em {str(edited_at)[:16]} | original: "{orig[:300]}"] '
    return marks


def _receipts(message_id: str, chat_jid: str) -> list:
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        try:
            return conn.execute('SELECT recipient, delivered_at, read_at, played_at FROM message_receipts '
                                'WHERE message_id = ? AND chat_jid = ? ORDER BY recipient', (message_id, chat_jid)).fetchall()
        finally:
            conn.close()
    except sqlite3.Error:
        return []  # bridge older than the receipts table


def _receipt_mark(message_id: str, chat_jid: str) -> str:
    """Status of a message WE sent, like WhatsApp's ticks. Nothing when there is no receipt yet (or the message
    is older than receipt tracking) - absence is not proof of non-delivery."""
    rows = _receipts(message_id, chat_jid)
    if not rows:
        return ''
    delivered = sum(1 for _, d, r, p in rows if d or r or p)
    read = sum(1 for _, _, r, p in rows if r or p)
    if chat_jid.endswith('@g.us'):
        return f'[entregue a {delivered}, lida por {read}] '
    _, d, r, p = rows[0]
    if p:
        return f'[OUVIDA/VISTA em {str(p)[:16]}] '
    if r:
        return f'[LIDA em {str(r)[:16]}] '
    return f'[ENTREGUE em {str(d)[:16]}, não lida] '


def _reactions(message_id: str, chat_jid: str) -> list:
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        try:
            return conn.execute('SELECT reactor, from_me, emoji, reacted_at FROM message_reactions '
                                'WHERE message_id = ? AND chat_jid = ? ORDER BY reacted_at', (message_id, chat_jid)).fetchall()
        finally:
            conn.close()
    except sqlite3.Error:
        return []  # bridge older than the reactions table


def _reaction_mark(message_id: str, chat_jid: str) -> str:
    """Emoji reactions on a message, with who reacted: [REAÇÕES: 👍 Maria, ❤️ Eu]."""
    rows = _reactions(message_id, chat_jid)
    if not rows:
        return ''
    who = [f"{emoji} {'Eu' if from_me else get_sender_name(reactor)}" for reactor, from_me, emoji, _ in rows]
    return f"[REAÇÕES: {', '.join(who)}] "


def message_status(chat_jid: str, message_id: str) -> dict:
    rows = _receipts(message_id, chat_jid)
    fmt = lambda t: str(t)[:19] if t else None
    return {
        'message_id': message_id, 'chat_jid': chat_jid,
        'recipients': [{'phone': who, 'name': get_sender_name(who), 'delivered_at': fmt(d or r or p), 'read_at': fmt(r or p),
                        'played_at': fmt(p)} for who, d, r, p in rows],
        'note': 'Sem linha para alguém = ainda não entregue no aparelho dele (ou mensagem anterior a 14/10/2026, quando o '
                'registro começou). "Entregue e não lida" pode ser lida com confirmação de leitura desligada.',
    }


def format_message(message: Message, show_chat_info: bool = True) -> None:
    """Print a single message with consistent formatting."""
    output = ""
    
    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "
        
    # The ID is shown for every message so Claude can reply to (quote) it with send_message(reply_to=...).
    if hasattr(message, 'media_type') and message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "
    else:
        content_prefix = f"[Message ID: {message.id}] "
    
    content_prefix += _edit_delete_marks(message.id, message.chat_jid)
    if message.is_from_me:
        content_prefix += _receipt_mark(message.id, message.chat_jid)
    content_prefix += _reaction_mark(message.id, message.chat_jid)
    try:
        sender_name = get_sender_name(message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        print(f"Error formatting message: {e}")
    return output

def format_messages_list(messages: List[Message], show_chat_info: bool = True) -> None:
    output = ""
    if not messages:
        output += "No messages to display."
        return output
    
    for message in messages:
        output += format_message(message, show_chat_info)
    return output

def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> List[Message]:
    """Get messages matching the specified criteria with optional context."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type FROM messages"]
        query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
        where_clauses = []
        params = []
        
        # Add filters
        if after:
            try:
                after = datetime.fromisoformat(after)
            except ValueError:
                raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp > ?")
            params.append(after)

        if before:
            try:
                before = datetime.fromisoformat(before)
            except ValueError:
                raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp < ?")
            params.append(before)

        if sender_phone_number:
            where_clauses.append("messages.sender = ?")
            params.append(sender_phone_number)
            
        if chat_jid:
            where_clauses.append("messages.chat_jid = ?")
            params.append(chat_jid)
            
        if query:
            where_clauses.append("LOWER(messages.content) LIKE LOWER(?)")
            params.append(f"%{query}%")
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add pagination
        offset = page * limit
        query_parts.append("ORDER BY messages.timestamp DESC")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        messages = cursor.fetchall()
        
        result = []
        for msg in messages:
            message = Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            )
            result.append(message)
            
        if include_context and result:
            # Add context for each message
            messages_with_context = []
            for msg in result:
                context = get_message_context(msg.id, context_before, context_after)
                messages_with_context.extend(context.before)
                messages_with_context.append(context.message)
                messages_with_context.extend(context.after)
            
            return format_messages_list(messages_with_context, show_chat_info=True)
            
        # Format and display messages without context
        return format_messages_list(result, show_chat_info=True)    
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> MessageContext:
    """Get context around a specific message."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Get the target message first
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.chat_jid, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.id = ?
        """, (message_id,))
        msg_data = cursor.fetchone()
        
        if not msg_data:
            raise ValueError(f"Message with ID {message_id} not found")
            
        target_message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[8]
        )
        
        # Get messages before
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp < ?
            ORDER BY messages.timestamp DESC
            LIMIT ?
        """, (msg_data[7], msg_data[0], before))
        
        before_messages = []
        for msg in cursor.fetchall():
            before_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        # Get messages after
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp > ?
            ORDER BY messages.timestamp ASC
            LIMIT ?
        """, (msg_data[7], msg_data[0], after))
        
        after_messages = []
        for msg in cursor.fetchall():
            after_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        return MessageContext(
            message=target_message,
            before=before_messages,
            after=after_messages
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        raise
    finally:
        if 'conn' in locals():
            conn.close()


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get chats matching the specified criteria."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        _register_fold(conn)
        cursor = conn.cursor()
        
        # Build base query
        last_cols = ("messages.content, messages.sender, messages.is_from_me"
                     if include_last_message else "NULL, NULL, NULL")
        query_parts = [f"""
            SELECT
                chats.jid,
                chats.name,
                chats.last_message_time,
                {last_cols}
            FROM chats
        """]
        
        if include_last_message:
            query_parts.append("""
                LEFT JOIN messages ON chats.jid = messages.chat_jid 
                AND chats.last_message_time = messages.timestamp
            """)
            
        where_clauses = []
        params = []
        
        if query:
            where_clauses.append("(fold(chats.name) LIKE ? OR chats.jid LIKE ?)")
            params.extend([f"%{_fold(query)}%", f"%{query}%"])
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add sorting
        order_by = "chats.last_message_time DESC" if sort_by == "last_active" else "chats.name"
        query_parts.append(f"ORDER BY {order_by}")
        
        # Add pagination
        offset = (page ) * limit
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        chats = cursor.fetchall()
        
        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def search_contacts(query: str) -> List[Contact]:
    """Search contacts by name or phone number — in synced chats AND in the WhatsApp address book
    (people you never chatted with since install only exist in the address book). Accent-insensitive."""
    pattern = '%' + _fold(query) + '%'
    found = {}  # jid -> name

    def add(jid, name):
        if jid and jid not in found:
            found[jid] = name

    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        _register_fold(conn)
        for jid, name in conn.execute("""
                SELECT jid, name FROM chats
                WHERE (fold(name) LIKE ? OR jid LIKE ?) AND jid NOT LIKE '%@g.us'
                ORDER BY last_message_time DESC LIMIT 50""", (pattern, pattern)):
            add(jid, name)
        conn.close()
    except sqlite3.Error as e:
        print(f"Database error (chats): {e}")

    try:
        conn = sqlite3.connect(f'file:{WHATSAPP_DB_PATH}?mode=ro', uri=True)
        _register_fold(conn)
        lid_to_pn = dict(conn.execute('SELECT lid, pn FROM whatsmeow_lid_map'))
        rows = conn.execute("""
                SELECT their_jid, COALESCE(NULLIF(full_name,''), NULLIF(first_name,''), NULLIF(business_name,''), push_name)
                FROM whatsmeow_contacts
                WHERE fold(COALESCE(full_name,'') || ' ' || COALESCE(first_name,'') || ' ' ||
                           COALESCE(push_name,'') || ' ' || COALESCE(business_name,'')) LIKE ?
                   OR their_jid LIKE ?
                LIMIT 100""", (pattern, pattern)).fetchall()
        conn.close()
        for jid, name in rows:
            user, _, server = jid.partition('@')
            user = user.split(':')[0]                     # drop device suffix (e.g. '1796...:38@lid')
            jid = f'{user}@{server}'
            if server == 'lid' and user in lid_to_pn:   # prefer the phone-number JID (what chats/send use)
                jid = f'{lid_to_pn[user]}@s.whatsapp.net'
            add(jid, name)
    except sqlite3.Error as e:
        print(f"Database error (address book): {e}")

    return [Contact(phone_number=jid.split('@')[0], name=name, jid=jid) for jid, name in list(found.items())[:50]]


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT DISTINCT
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
            JOIN messages m ON c.jid = m.chat_jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY c.last_message_time DESC
            LIMIT ? OFFSET ?
        """, (jid, jid, limit, page * limit))
        
        chats = cursor.fetchall()
        
        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_last_interaction(jid: str) -> str:
    """Get most recent message involving the contact."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT 
                m.timestamp,
                m.sender,
                c.name,
                m.content,
                m.is_from_me,
                c.jid,
                m.id,
                m.media_type
            FROM messages m
            JOIN chats c ON m.chat_jid = c.jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY m.timestamp DESC
            LIMIT 1
        """, (jid, jid))
        
        msg_data = cursor.fetchone()
        
        if not msg_data:
            return None
            
        message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[7]
        )
        
        return format_message(message)
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        query = """
            SELECT 
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
        """
        
        if include_last_message:
            query += """
                LEFT JOIN messages m ON c.jid = m.chat_jid 
                AND c.last_message_time = m.timestamp
            """
            
        query += " WHERE c.jid = ?"
        
        cursor.execute(query, (chat_jid,))
        chat_data = cursor.fetchone()
        
        if not chat_data:
            return None
            
        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get chat metadata by sender phone number."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT 
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
            LEFT JOIN messages m ON c.jid = m.chat_jid 
                AND c.last_message_time = m.timestamp
            WHERE c.jid LIKE ? AND c.jid NOT LIKE '%@g.us'
            LIMIT 1
        """, (f"%{sender_phone_number}%",))
        
        chat_data = cursor.fetchone()
        
        if not chat_data:
            return None
            
        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()

def send_message(recipient: str, message: str, reply_to: Optional[str] = None,
                 link_preview: bool = True) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "message": message,
            "link_preview": bool(link_preview),
        }
        if reply_to:
            payload["reply_to"] = reply_to
        
        response = requests.post(url, json=payload)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def save_contact(phone: str, full_name: str, first_name: str = "", save_on_phone: bool = True) -> Tuple[bool, str]:
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/contact", json={
            "phone": phone, "full_name": full_name, "first_name": first_name, "save_on_phone": save_on_phone}, timeout=60)
        result = response.json()
        return result.get("success", False), result.get("message", f"HTTP {response.status_code}")
    except requests.RequestException as e:
        return False, f"Bridge not reachable (is whatsapp-bridge running?): {e}"
    except ValueError:
        return False, f"HTTP {response.status_code} - {response.text[:300]}"

def send_reaction(chat_jid: str, message_id: str, emoji: str) -> Tuple[bool, str]:
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/react",
                                 json={"chat_jid": chat_jid, "message_id": message_id, "emoji": emoji}, timeout=30)
        result = response.json()
        return result.get("success", False), result.get("message", f"HTTP {response.status_code}")
    except requests.RequestException as e:
        return False, f"Bridge not reachable (is whatsapp-bridge running?): {e}"
    except ValueError:
        return False, f"HTTP {response.status_code} - {response.text[:300]}"

def delete_messages(chat_jid: str, message_ids: list, scope: str) -> Tuple[bool, str]:
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/delete",
                                 json={"chat_jid": chat_jid, "message_ids": list(message_ids), "scope": scope},
                                 timeout=60 + 5 * len(message_ids))
        result = response.json()
        return result.get("success", False), result.get("message", f"HTTP {response.status_code}")
    except requests.RequestException as e:
        return False, f"Bridge not reachable (is whatsapp-bridge running?): {e}"
    except ValueError:
        return False, f"HTTP {response.status_code} - {response.text[:300]}"

def star_messages(chat_jid: str, message_ids: list, starred: bool = True) -> Tuple[bool, str]:
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/star",
                                 json={"chat_jid": chat_jid, "message_ids": list(message_ids), "starred": starred},
                                 timeout=60)
        result = response.json()
        return result.get("success", False), result.get("message", f"HTTP {response.status_code}")
    except requests.RequestException as e:
        return False, f"Bridge not reachable (is whatsapp-bridge running?): {e}"
    except ValueError:
        return False, f"HTTP {response.status_code} - {response.text[:300]}"

def pin_message(chat_jid: str, message_id: str, pin: bool = True, duration: str = "7d") -> Tuple[bool, str]:
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/pin",
                                 json={"chat_jid": chat_jid, "message_id": message_id, "pin": pin, "duration": duration},
                                 timeout=60)
        result = response.json()
        return result.get("success", False), result.get("message", f"HTTP {response.status_code}")
    except requests.RequestException as e:
        return False, f"Bridge not reachable (is whatsapp-bridge running?): {e}"
    except ValueError:
        return False, f"HTTP {response.status_code} - {response.text[:300]}"

def request_chat_history(chat_jid: str, count: int = 50, until_date: Optional[str] = None) -> Tuple[bool, str]:
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/history",
                                 json={"chat_jid": chat_jid, "count": count, "before": until_date or ""}, timeout=30)
        result = response.json()
        return result.get("success", False), result.get("message", f"HTTP {response.status_code}")
    except requests.RequestException as e:
        return False, f"Bridge not reachable (is whatsapp-bridge running?): {e}"
    except ValueError:
        return False, f"HTTP {response.status_code} - {response.text[:300]}"

def mark_chat_as_read(chat_jid: str) -> Tuple[bool, str]:
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/markread", json={"chat_jid": chat_jid}, timeout=30)
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        return False, f"Error: HTTP {response.status_code} - {response.text}"
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"

def send_file(recipient: str, media_path: str, caption: str = "", reply_to: Optional[str] = None,
              as_document: bool = False, hd: bool = True) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        if not media_path:
            return False, "Media path must be provided"
        
        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path,
            "message": caption or "",
            "as_document": bool(as_document),
            "hd": bool(hd),
        }
        if reply_to:
            payload["reply_to"] = reply_to
        
        response = requests.post(url, json=payload)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"
        
        if not media_path:
            return False, "Media path must be provided"
        
        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        if not media_path.endswith(".ogg"):
            try:
                media_path = audio.convert_to_opus_ogg_temp(media_path)
            except Exception as e:
                return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}"
        
        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path
        }
        
        response = requests.post(url, json=payload)
        
        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"
            
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def download_media(message_id: str, chat_jid: str) -> Tuple[Optional[str], str]:
    """Download media from a message.

    Returns:
        (local file path or None, status message) — the message carries the bridge's real error
        so Claude can tell "expired/forbidden" from "not a media message" instead of a bare failure.
    """
    try:
        url = f"{WHATSAPP_API_BASE_URL}/download"
        payload = {
            "message_id": message_id,
            "chat_jid": chat_jid
        }

        response = requests.post(url, json=payload, timeout=300)
        try:
            result = response.json()
        except ValueError:
            return None, f"HTTP {response.status_code} - {response.text[:300]}"
        if response.status_code == 200 and result.get("success", False):
            return result.get("path"), result.get("message", "Media downloaded successfully")
        return None, result.get("message", f"HTTP {response.status_code}")
    except requests.RequestException as e:
        return None, f"Bridge not reachable (is whatsapp-bridge running?): {e}"


def backfill_chat_history(chat_jid: str, since: Optional[str] = None) -> Tuple[bool, str]:
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/backfill", json={"chat_jid": chat_jid, "since": since or ""}, timeout=30)
        result = response.json()
        return result.get("success", False), result.get("message", f"HTTP {response.status_code}")
    except requests.RequestException as e:
        return False, f"Bridge not reachable (is whatsapp-bridge running?): {e}"
    except ValueError:
        return False, f"HTTP {response.status_code} - {response.text[:300]}"


def backfill_status(chat_jid: Optional[str] = None) -> Any:
    try:
        response = requests.get(f"{WHATSAPP_API_BASE_URL}/backfill", params={"chat_jid": chat_jid or ""}, timeout=30)
        return response.json()
    except requests.RequestException as e:
        return f"Bridge not reachable (is whatsapp-bridge running?): {e}"
    except ValueError:
        return f"HTTP {response.status_code} - {response.text[:300]}"

def _group_call(path: str, payload: Dict[str, Any], timeout: int = 90) -> Dict[str, Any]:
    """POST to a /api/group/* endpoint; drops None fields so only what was given is changed."""
    try:
        body = {k: v for k, v in payload.items() if v is not None}
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/group/{path}", json=body, timeout=timeout)
        try:
            return response.json()
        except ValueError:
            return {"success": False, "message": f"HTTP {response.status_code} - {response.text[:300]}"}
    except requests.RequestException as e:
        return {"success": False, "message": f"Bridge not reachable (is whatsapp-bridge running?): {e}"}

def create_group(name: str, participants: List[str], description: Optional[str] = None,
                 photo_path: Optional[str] = None, **permissions: Optional[bool]) -> Dict[str, Any]:
    return _group_call("create", {"name": name, "participants": participants, "description": description,
                                  "photo_path": photo_path, **permissions}, timeout=180)

def update_group_settings(chat_jid: str, **fields: Any) -> Dict[str, Any]:
    return _group_call("settings", {"chat_jid": chat_jid, **fields})

def update_group_members(chat_jid: str, action: str, participants: List[str]) -> Dict[str, Any]:
    return _group_call("members", {"chat_jid": chat_jid, "action": action, "participants": participants}, timeout=180)

def group_info(chat_jid: str, include_invite_link: bool = False, reset_invite_link: bool = False) -> Dict[str, Any]:
    return _group_call("info", {"chat_jid": chat_jid, "include_invite_link": include_invite_link,
                                "reset_invite_link": reset_invite_link})

def send_contact(recipient: str, contacts: List[Dict[str, Any]], reply_to: Optional[str] = None) -> Tuple[bool, str]:
    try:
        response = requests.post(f"{WHATSAPP_API_BASE_URL}/send_contact",
                                 json={"recipient": recipient, "contacts": contacts, "reply_to": reply_to or ""}, timeout=60)
        result = response.json()
        return result.get("success", False), result.get("message", f"HTTP {response.status_code}")
    except requests.RequestException as e:
        return False, f"Bridge not reachable (is whatsapp-bridge running?): {e}"
    except ValueError:
        return False, f"HTTP {response.status_code} - {response.text[:300]}"
