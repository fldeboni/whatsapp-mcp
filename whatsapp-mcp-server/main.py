import builtins, functools, sys
# stdout is the MCP stdio channel: any stray print() from whatsapp.py/audio.py would corrupt it.
builtins.print = functools.partial(builtins.print, file=sys.stderr)

import dataclasses
from datetime import datetime
from typing import List, Dict, Any, Optional
from mcp.server.fastmcp import FastMCP


def _plain(value):
    """mcp>=1.10 validates tool output against the return annotation: dataclasses (Chat, Contact, ...)
    and datetimes must become plain dicts/strings, or every call fails with 'Input should be a valid dictionary'."""
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        return {k: _plain(v) for k, v in dataclasses.asdict(value).items()}
    if isinstance(value, datetime):
        return value.isoformat()
    if isinstance(value, list):
        return [_plain(v) for v in value]
    if isinstance(value, dict):
        return {k: _plain(v) for k, v in value.items()}
    return value
from whatsapp import (
    search_contacts as whatsapp_search_contacts,
    list_messages as whatsapp_list_messages,
    list_chats as whatsapp_list_chats,
    get_chat as whatsapp_get_chat,
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
    get_contact_chats as whatsapp_get_contact_chats,
    get_last_interaction as whatsapp_get_last_interaction,
    get_message_context as whatsapp_get_message_context,
    send_message as whatsapp_send_message,
    send_file as whatsapp_send_file,
    send_audio_message as whatsapp_audio_voice_message,
    download_media as whatsapp_download_media,
    mark_chat_as_read as whatsapp_mark_chat_as_read,
    request_chat_history as whatsapp_request_chat_history,
    backfill_chat_history as whatsapp_backfill_chat_history,
    backfill_status as whatsapp_backfill_status,
    message_status as whatsapp_message_status,
    send_reaction as whatsapp_send_reaction,
    delete_messages as whatsapp_delete_messages,
    star_messages as whatsapp_star_messages,
    save_contact as whatsapp_save_contact,
    create_group as whatsapp_create_group,
    update_group_settings as whatsapp_update_group_settings,
    update_group_members as whatsapp_update_group_members,
    group_info as whatsapp_group_info,
    send_contact as whatsapp_send_contact
)

# Initialize FastMCP server
mcp = FastMCP("whatsapp")

@mcp.tool()
def search_contacts(query: str) -> List[Dict[str, Any]]:
    """Search WhatsApp contacts by name or phone number.
    
    Args:
        query: Search term to match against contact names or phone numbers
    """
    contacts = whatsapp_search_contacts(query)
    return _plain(contacts)

@mcp.tool()
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
) -> str:
    """Get WhatsApp messages matching specified criteria with optional context.
    
    Args:
        after: Optional ISO-8601 formatted string to only return messages after this date
        before: Optional ISO-8601 formatted string to only return messages before this date
        sender_phone_number: Optional phone number to filter messages by sender
        chat_jid: Optional chat JID to filter messages by chat
        query: Optional search term to filter messages by content
        limit: Maximum number of messages to return (default 20)
        page: Page number for pagination (default 0)
        include_context: Whether to include messages before and after matches (default True)
        context_before: Number of messages to include before each match (default 1)
        context_after: Number of messages to include after each match (default 1)
    """
    messages = whatsapp_list_messages(
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after
    )
    return messages

@mcp.tool()
def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Dict[str, Any]]:
    """Get WhatsApp chats matching specified criteria.
    
    Args:
        query: Optional search term to filter chats by name or JID
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
        include_last_message: Whether to include the last message in each chat (default True)
        sort_by: Field to sort results by, either "last_active" or "name" (default "last_active")
    """
    chats = whatsapp_list_chats(
        query=query,
        limit=limit,
        page=page,
        include_last_message=include_last_message,
        sort_by=sort_by
    )
    return _plain(chats)

@mcp.tool()
def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Dict[str, Any]]:
    """Get WhatsApp chat metadata by JID.
    
    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)
    """
    chat = whatsapp_get_chat(chat_jid, include_last_message)
    return _plain(chat)

@mcp.tool()
def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Dict[str, Any]]:
    """Get WhatsApp chat metadata by sender phone number.
    
    Args:
        sender_phone_number: The phone number to search for
    """
    chat = whatsapp_get_direct_chat_by_contact(sender_phone_number)
    return _plain(chat)

@mcp.tool()
def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Dict[str, Any]]:
    """Get all WhatsApp chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    chats = whatsapp_get_contact_chats(jid, limit, page)
    return _plain(chats)

@mcp.tool()
def get_last_interaction(jid: str) -> str:
    """Get most recent WhatsApp message involving the contact.
    
    Args:
        jid: The JID of the contact to search for
    """
    message = whatsapp_get_last_interaction(jid)
    return message

@mcp.tool()
def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> Optional[Dict[str, Any]]:
    """Get context around a specific WhatsApp message.
    
    Args:
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
    """
    context = whatsapp_get_message_context(message_id, before, after)
    return _plain(context)

@mcp.tool()
def send_message(
    recipient: str,
    message: str,
    reply_to: Optional[str] = None
) -> Dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        message: The message text to send. To MENTION someone in a group, write "@" + their phone number with
                 country code (e.g. "@5527999999999") in the text: it becomes a real mention (name highlighted,
                 person notified). Do not write "@Name".
        reply_to: Optional message ID to REPLY to (quoted above the text, like "Responder" in the app). Get it from
                  list_messages ("Message ID: ..."); the message must belong to the same chat as recipient.
    
    Returns:
        A dictionary containing success status and a status message
    """
    # Validate input
    if not recipient:
        return {
            "success": False,
            "message": "Recipient must be provided"
        }
    
    # Call the whatsapp_send_message function with the unified recipient parameter
    success, status_message = whatsapp_send_message(recipient, message, reply_to)
    return {
        "success": success,
        "message": status_message
    }

@mcp.tool()
def send_file(recipient: str, media_path: str, caption: str = "", reply_to: Optional[str] = None,
              as_document: bool = False, hd: bool = True) -> Dict[str, Any]:
    """Send a file such as a picture, raw audio, video or document via WhatsApp to the specified recipient. For group messages use the JID.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the media file to send (image, video, document). Videos (.mp4, .mov,
                 .m4v, .mkv, .webm, .3gp, .avi) go with thumbnail and duration; if they would not play on every
                 phone (HEVC, AVI...) or pass 60 MB the bridge converts/compresses them to MP4 H.264 (needs
                 ffmpeg) and the result says so.
        caption: Optional text shown with the image/video/document (the "legenda"). Mentions work like in
                 send_message: "@" + phone with country code (e.g. "@5527999999999") in a group.
        reply_to: Optional message ID to reply to (quoted above), from list_messages.
        as_document: True sends the file as a DOCUMENT: original bytes, no compression/conversion, up to 2 GB
                 (recipient taps to open; no preview in the chat). Use for "mandar como documento" / original quality.
        hd: Photos (jpg/png > 1280 px) and videos (> 480p) go as WhatsApp HD (badge "HD", original quality) by
                 default; False sends them in standard quality only.
    
    Returns:
        A dictionary containing success status and a status message
    """
    
    # Call the whatsapp_send_file function
    success, status_message = whatsapp_send_file(recipient, media_path, caption, reply_to, as_document, hd)
    return {
        "success": success,
        "message": status_message
    }

@mcp.tool()
def send_audio_message(recipient: str, media_path: str) -> Dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified recipient. For group messages use the JID. If it errors due to ffmpeg not being installed, use send_file instead.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the audio file to send (will be converted to Opus .ogg if it's not a .ogg file)
    
    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_audio_voice_message(recipient, media_path)
    return {
        "success": success,
        "message": status_message
    }

@mcp.tool()
def mark_chats_as_read(chat_jids: List[str]) -> List[Dict[str, Any]]:
    """Mark one or more WhatsApp chats as read (clears the unread badge on the phone and other devices).
    Use list_chats to find the JIDs; group JIDs end with @g.us.

    Args:
        chat_jids: List of chat JIDs to mark as read
    """
    # Human-like pacing: a person opens/marks one chat every few seconds, never a burst.
    # Random order and jittered gaps (4-12s, occasionally a longer 15-30s pause) keep it from looking scripted.
    import random, time
    jids = list(dict.fromkeys(chat_jids))
    random.shuffle(jids)
    results = []
    for i, jid in enumerate(jids):
        if i > 0:
            time.sleep(random.uniform(15, 30) if random.random() < 0.15 else random.uniform(4, 12))
        success, status_message = whatsapp_mark_chat_as_read(jid)
        results.append({"chat_jid": jid, "success": success, "message": status_message})
    return results

@mcp.tool()
def save_contact(phone: str, full_name: str, first_name: str = "", save_on_phone: bool = True) -> Dict[str, Any]:
    """Create or update a WhatsApp contact (same as "New contact"/"Edit contact" in WhatsApp; synced to all devices).
    Saving again with the same phone updates the name. The number must be on WhatsApp.

    Args:
        phone: Phone number with country code, digits only (e.g. "5527999999999")
        full_name: Name to save (e.g. "Maria Silva (Fornecedor X)")
        first_name: Optional first name; defaults to the first word of full_name
        save_on_phone: Also save in the phone's own address book (default True)
    """
    success, status_message = whatsapp_save_contact(phone, full_name, first_name, save_on_phone)
    return {"success": success, "message": status_message}

@mcp.tool()
def send_reaction(chat_jid: str, message_id: str, emoji: str) -> Dict[str, Any]:
    """React to a WhatsApp message with an emoji (visible to everyone in the chat). Use an empty string to remove
    your reaction. Get the message ID from list_messages ("Message ID: ...").

    Args:
        chat_jid: The chat JID the message belongs to (person "...@s.whatsapp.net" or group "...@g.us")
        message_id: The ID of the message to react to
        emoji: A single emoji, e.g. "👍", "❤️", "😂", "🙏"; "" removes the reaction
    """
    success, status_message = whatsapp_send_reaction(chat_jid, message_id, emoji)
    return {"success": success, "message": status_message}

@mcp.tool()
def delete_message(chat_jid: str, message_ids: List[str], scope: str) -> Dict[str, Any]:
    """Delete one or more messages of ONE chat, like the phone does. Always confirm with the user first, showing
    the chat and the text of each message that will be deleted. Get IDs from list_messages ("Message ID: ...").

    Args:
        chat_jid: The chat JID (person "...@s.whatsapp.net" or group "...@g.us")
        message_ids: IDs of the messages to delete (several at once is fine, e.g. for a cleanup)
        scope: "everyone" = "Apagar para todos" (my own messages up to ~60 h after sending; other people's messages
               only in a group where I am admin; everyone sees "Mensagem apagada").
               "me" = "Apagar para mim" (any message, any age; disappears only from my phone/devices, nobody is
               notified).
    The local copy keeps the text: list_messages shows [APAGADA PELO REMETENTE ...] / [APAGADA SÓ PARA MIM ...].
    """
    success, status_message = whatsapp_delete_messages(chat_jid, message_ids, scope)
    return {"success": success, "message": status_message}

@mcp.tool()
def star_message(chat_jid: str, message_ids: List[str], starred: bool = True) -> Dict[str, Any]:
    """Star ("Favoritar") or unstar messages of ONE chat. Only for me: syncs to "Mensagens favoritas" on the
    phone, nobody else sees it. Starred messages show [FAVORITA] in list_messages. IDs come from list_messages.

    Args:
        chat_jid: The chat JID (person "...@s.whatsapp.net" or group "...@g.us")
        message_ids: IDs of the messages (several at once is fine)
        starred: True to star (default), False to remove the star
    """
    success, status_message = whatsapp_star_messages(chat_jid, message_ids, starred)
    return {"success": success, "message": status_message}

@mcp.tool()
def send_contact(recipient: str, contacts: List[Dict[str, Any]], reply_to: Optional[str] = None) -> Dict[str, Any]:
    """Send one or more contact cards (vCard), like "Share contact" in WhatsApp: the recipient sees the card with
    "Message" / "Add contact" buttons. Received contact cards show up in list_messages as "[CONTATO] ...".

    Args:
        recipient: Phone with country code (digits only) or a JID (person "...@s.whatsapp.net" or group "...@g.us")
        contacts: List of contacts, each {"name": "Maria Silva", "phones": ["5527999999999"], optional
                  "emails": ["maria@empresa.com"], "company": "Empresa", "title": "Compras", "note": "..."}
                  Phones: digits with country code (each becomes a WhatsApp-linked number on the card)
        reply_to: Optional message ID to reply to (quoted above), from list_messages
    """
    success, status_message = whatsapp_send_contact(recipient, contacts, reply_to)
    return {"success": success, "message": status_message}

@mcp.tool()
def get_group_info(chat_jid: str, include_invite_link: bool = False, reset_invite_link: bool = False) -> Dict[str, Any]:
    """Current info of a group, straight from WhatsApp: name, description, members (phone, name, role: criador/admin/
    membro), whether I am admin, and the permission settings. Optionally the invite link (I must be admin).

    Args:
        chat_jid: The group JID ("...@g.us"; find it with list_chats)
        include_invite_link: Also return the group's invite link (https://chat.whatsapp.com/...)
        reset_invite_link: Revoke the current link and return a new one (the old link stops working)
    """
    return whatsapp_group_info(chat_jid, include_invite_link, reset_invite_link)

@mcp.tool()
def create_group(
    name: str,
    participants: List[str],
    description: Optional[str] = None,
    photo_path: Optional[str] = None,
    only_admins_send: Optional[bool] = None,
    only_admins_edit: Optional[bool] = None,
    only_admins_add: Optional[bool] = None,
    approve_new_members: Optional[bool] = None
) -> Dict[str, Any]:
    """Create a WhatsApp group (I become its admin). Members are notified. Avoid adding many unknown numbers at
    once (ban risk). People whose privacy blocks being added are reported in "not_added": send them the invite
    link (get_group_info with include_invite_link=True).

    Args:
        name: Group name (up to 100 characters)
        participants: Phone numbers with country code, digits only (e.g. ["5527999999999"]); I'm added automatically
        description: Optional group description
        photo_path: Optional absolute path to a JPEG/PNG for the group picture (cropped to a square automatically)
        only_admins_send: True = only admins can send messages
        only_admins_edit: True = only admins can edit name/description/photo
        only_admins_add: True = only admins can add members
        approve_new_members: True = admins must approve people joining via link
    """
    return whatsapp_create_group(name, participants, description, photo_path,
                                 only_admins_send=only_admins_send, only_admins_edit=only_admins_edit,
                                 only_admins_add=only_admins_add, approve_new_members=approve_new_members)

@mcp.tool()
def update_group_settings(
    chat_jid: str,
    name: Optional[str] = None,
    description: Optional[str] = None,
    photo_path: Optional[str] = None,
    remove_photo: bool = False,
    only_admins_send: Optional[bool] = None,
    only_admins_edit: Optional[bool] = None,
    only_admins_add: Optional[bool] = None,
    approve_new_members: Optional[bool] = None
) -> Dict[str, Any]:
    """Change a group's name, description, picture and/or permissions. Only the fields given are changed.
    Usually requires being an admin (check get_group_info "i_am_admin"). Members see a notice of each change.

    Args:
        chat_jid: The group JID ("...@g.us")
        name: New group name
        description: New description ("" removes it)
        photo_path: Absolute path to a JPEG/PNG for the new picture (cropped to a square automatically)
        remove_photo: True removes the current picture
        only_admins_send: True = only admins can send messages; False = everyone
        only_admins_edit: True = only admins can edit group info; False = everyone
        only_admins_add: True = only admins can add members; False = everyone
        approve_new_members: True = admins approve people joining via link; False = join directly
    """
    return whatsapp_update_group_settings(chat_jid, name=name, description=description, photo_path=photo_path,
                                          remove_photo=remove_photo, only_admins_send=only_admins_send,
                                          only_admins_edit=only_admins_edit, only_admins_add=only_admins_add,
                                          approve_new_members=approve_new_members)

@mcp.tool()
def update_group_members(chat_jid: str, action: str, participants: List[str]) -> Dict[str, Any]:
    """Add, remove, promote to admin or demote from admin members of a group (I must be admin). Everyone in the
    group sees it. For "add", people whose privacy blocks it come back in "failed": send them the invite link.

    Args:
        chat_jid: The group JID ("...@g.us")
        action: "add", "remove", "promote" (make admin) or "demote" (remove admin)
        participants: Phone numbers with country code, digits only (e.g. ["5527999999999"])
    """
    return whatsapp_update_group_members(chat_jid, action, participants)

@mcp.tool()
def request_chat_history(chat_jid: str, count: int = 50, until_date: Optional[str] = None) -> Dict[str, Any]:
    """Fetch OLDER messages of one chat from the phone (history from before the MCP was connected is incomplete).
    Asks the phone for up to `count` (max 50) messages older than the oldest one stored (or the latest ones,
    if the chat has nothing stored yet); they arrive a few seconds later. Then call list_messages again.
    Repeat to go further back. The phone must be online.

    GAPS: a chat can miss days in the MIDDLE (bridge was off) even though older and newer messages exist - a
    plain request never fills those. If a day/period has no messages but the user says there are, call this with
    until_date = the last day of the gap (YYYY-MM-DD): the phone sends the messages up to that day.

    Args:
        chat_jid: The chat JID (e.g. "5527999999999@s.whatsapp.net" or a group "...@g.us")
        count: How many older messages to request (1-50, default 50)
        until_date: Optional YYYY-MM-DD; fetch messages up to and including this day (fills a gap)
    """
    success, status_message = whatsapp_request_chat_history(chat_jid, count, until_date)
    return {"success": success, "message": status_message}

@mcp.tool()
def get_message_status(chat_jid: str, message_id: str) -> Dict[str, Any]:
    """Was a message I sent delivered / read? Per recipient (groups list each member that confirmed), with times.
    list_messages already shows a short mark on my own messages ([LIDA em ...], [ENTREGUE em ..., não lida],
    [entregue a N, lida por M]). Read receipts only exist if the recipient has them on.

    Args:
        chat_jid: The chat JID
        message_id: The ID of a message sent by me
    """
    return whatsapp_message_status(chat_jid, message_id)

@mcp.tool()
def backfill_chat_history(chat_jid: str, since: Optional[str] = None) -> Dict[str, Any]:
    """Pull the WHOLE older history of one chat from the phone, in the background (batches of 50 every ~20 s,
    paced like a human) until the date `since` or until the phone has nothing older. Use it when a chat's stored
    history is incomplete and the user agreed to pull it. Returns right away; check with backfill_status.

    Args:
        chat_jid: The chat JID (e.g. "5527999999999@s.whatsapp.net" or a group "...@g.us")
        since: Optional date YYYY-MM-DD to stop at; omit to go back as far as the phone has
    """
    success, status_message = whatsapp_backfill_chat_history(chat_jid, since)
    return {"success": success, "message": status_message}

@mcp.tool()
def backfill_status(chat_jid: Optional[str] = None) -> Dict[str, Any]:
    """Progress of backfill_chat_history: running, batches, messages stored, oldest message date, result.

    Args:
        chat_jid: The chat JID; omit to list every backfill since the bridge started
    """
    return {"status": whatsapp_backfill_status(chat_jid)}

@mcp.tool()
def download_media(message_id: str, chat_jid: str) -> Dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.
    
    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
    
    Returns:
        A dictionary containing success status, a status message, and the file path if successful
    """
    file_path, status_message = whatsapp_download_media(message_id, chat_jid)
    if file_path:
        return {"success": True, "message": status_message, "file_path": file_path}
    return {"success": False, "message": status_message}

if __name__ == "__main__":
    # Initialize and run the server
    mcp.run(transport='stdio')