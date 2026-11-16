"""Ponte WhatsApp <-> chat do Claude desktop.

Cada grupo do WhatsApp "ligado" corresponde a um chat do Claude (sessão local_...). O que o dono do número manda
no grupo (texto, áudio transcrito, foto/arquivo) vira prompt naquele chat; a resposta final do chat volta para o
grupo, em nome do dono, começando por "*Retorno do Claude:*".

Divisão de trabalho:
- Este script lê o banco local do bridge, transcreve áudios, monta o prompt, acompanha a resposta na conversa do
  chat de destino e a envia ao grupo. Ele só fala com grupos ligados (nunca com outro chat do WhatsApp).
- Um chat do Claude ("Ponte") roda `ponte.py vigiar` num Monitor. A cada evento "entregar" ele repassa o prompt ao
  chat de destino (só um chat do Claude consegue escrever em outro) e roda `ponte.py entregue <lote>`.

Se a ponte estiver fora do ar, nada se perde: as mensagens ficam no banco do bridge e, quando ela voltar, vão
juntas num único prompt.

Configuração e estado ficam em %USERPROFILE%\\.whatsapp-mcp\\claude-ponte (fora do repositório).

Comandos:
  ponte.py ligar <grupo_jid> <sessao_id> <titulo>   liga um grupo a um chat (a partir de agora)
  ponte.py desligar <grupo_jid>
  ponte.py listar
  ponte.py vigiar                                   laço principal; imprime um evento JSON por linha
  ponte.py entregue <lote>                          o chat Ponte confirma que repassou o prompt
  ponte.py falhou <lote> <motivo>                   o chat Ponte não conseguiu repassar
"""
import glob
import json
import os
import re
import sqlite3
import subprocess
import sys
import time
import urllib.request
import uuid
from datetime import datetime

HOME = os.path.expanduser("~")
BASE = os.path.join(HOME, ".whatsapp-mcp", "claude-ponte")
LINKS = os.path.join(BASE, "links.json")
STATE = os.path.join(BASE, "state.json")
PROMPTS = os.path.join(BASE, "prompts")
LOCK = os.path.join(BASE, "vigiar.pid")
DB = os.path.join(HOME, ".whatsapp-mcp", "store", "messages.db")
API = "http://127.0.0.1:8080/api"
HERE = os.path.dirname(os.path.abspath(__file__))
WHISPER_PY = os.path.join(HOME, ".whatsapp-mcp", "whisper-venv", "Scripts", "python.exe")
SESSIONS_GLOB = os.path.join(os.environ.get("LOCALAPPDATA", ""), "Claude-Data", "*", "claude-code-sessions", "*", "*")
PREFIX = "*Retorno do Claude:*"
POLL_S = 3
SLOW_NOTICE_S = 20 * 60
MAX_CHUNK = 3500

sys.stdout.reconfigure(encoding="utf-8", line_buffering=True)


# ---------- arquivos de configuração e estado ----------

def load(path, default):
    try:
        with open(path, encoding="utf-8") as f:
            return json.load(f)
    except (FileNotFoundError, json.JSONDecodeError):
        return default


def save(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, indent=2)
    os.replace(tmp, path)


def links():
    return load(LINKS, {"links": []})["links"]


def emit(event, **kw):
    print(json.dumps({"evento": event, **kw}, ensure_ascii=False), flush=True)


def now_iso():
    return datetime.now().astimezone().isoformat(timespec="seconds")


# ---------- bridge ----------

def api(path, payload, timeout=120):
    req = urllib.request.Request(f"{API}/{path}", data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read() or b"{}")
        except ValueError:
            return {"success": False, "message": f"HTTP {e.code}"}
    except Exception as e:  # noqa: BLE001 - bridge down is a normal condition here
        return {"success": False, "message": f"bridge fora do ar: {e}"}


def react(group, msg_id, emoji):
    api("react", {"chat_jid": group, "message_id": msg_id, "emoji": emoji}, timeout=30)


def to_whatsapp(md: str) -> str:
    """Markdown do Claude -> formatação do WhatsApp."""
    out = []
    in_code = False
    for line in md.splitlines():
        if line.strip().startswith("```"):
            in_code = not in_code
            out.append("```")  # WhatsApp shows the language tag ("bash") as text: drop it
            continue
        if in_code:
            out.append(line)
            continue
        m = re.match(r"^\s{0,3}#{1,6}\s+(.*)$", line)
        if m:
            line = f"*{m.group(1).strip().strip('*')}*"
        line = re.sub(r"\*\*(.+?)\*\*", r"*\1*", line)
        line = re.sub(r"__(.+?)__", r"*\1*", line)
        line = re.sub(r"\[([^\]]+)\]\((https?://[^)]+)\)", r"\1 (\2)", line)
        line = re.sub(r"^(\s*)[-*]\s+", r"\1• ", line)
        out.append(line)
    return "\n".join(out).strip()


def send_group(group, text, state):
    chunks, cur = [], ""
    for para in text.split("\n"):
        if len(cur) + len(para) + 1 > MAX_CHUNK and cur:
            chunks.append(cur)
            cur = ""
        cur = f"{cur}\n{para}" if cur else para
    if cur:
        chunks.append(cur)
    ok = True
    for i, chunk in enumerate(chunks):
        body = f"{PREFIX}\n{chunk}" if i == 0 else f"{PREFIX} (continuação)\n{chunk}"
        r = api("send", {"recipient": group, "message": body, "link_preview": False})
        m = re.search(r"\(ID ([A-Za-z0-9]+)\)", r.get("message", ""))
        if m:
            state.setdefault("enviadas", []).append(m.group(1))
            state["enviadas"] = state["enviadas"][-500:]
        ok = ok and r.get("success", False)
        time.sleep(1.5)
    return ok


# ---------- mensagens novas ----------

def new_messages(group, cursor, sent_ids):
    con = sqlite3.connect(f"file:{DB}?mode=ro", uri=True, timeout=10)
    try:
        rows = con.execute(
            "SELECT id, timestamp, COALESCE(content,''), COALESCE(media_type,''), COALESCE(filename,'') "
            "FROM messages WHERE chat_jid = ? AND is_from_me = 1 AND (timestamp > ? OR (timestamp = ? AND id > ?)) "
            "ORDER BY timestamp, id", (group, cursor["ts"], cursor["ts"], cursor["id"])).fetchall()
    finally:
        con.close()
    out = []
    for mid, ts, content, mtype, fname in rows:
        if mid in sent_ids or content.startswith(PREFIX):
            continue
        out.append({"id": mid, "ts": ts, "texto": content, "tipo": mtype, "arquivo": fname})
    return out


def describe(group, m):
    """Texto que o chat de destino vai ler para uma mensagem."""
    hora = m["ts"][11:16]
    if not m["tipo"]:
        return f"[{hora}] {m['texto']}"
    r = api("download", {"message_id": m["id"], "chat_jid": group}, timeout=300)
    path = r.get("path") if r.get("success") else None
    if m["tipo"] == "audio":
        if not path:
            return f"[{hora}] (áudio que não consegui baixar: {r.get('message')})"
        t = subprocess.run([WHISPER_PY, os.path.join(HERE, "transcrever.py"), path],
                           capture_output=True, text=True, encoding="utf-8", timeout=900)
        if t.returncode == 0 and t.stdout.strip():
            return f"[{hora}] (áudio transcrito) {t.stdout.strip()}"
        return f"[{hora}] (áudio que não consegui transcrever: {t.stderr.strip()[:200]}; arquivo: {path})"
    legenda = f" — legenda: {m['texto']}" if m["texto"] else ""
    if path:
        return f"[{hora}] ({m['tipo']} enviado: {path}){legenda}"
    return f"[{hora}] ({m['tipo']} que não consegui baixar: {r.get('message')}){legenda}"


# ---------- resposta no chat de destino ----------

def transcript_for(session_id):
    for f in glob.glob(os.path.join(SESSIONS_GLOB, f"{session_id}.json")):
        info = load(f, {})
        for cli in {info.get("lastAssistantCliSessionId"), info.get("cliSessionId")} - {None}:
            hits = glob.glob(os.path.join(HOME, ".claude", "projects", "*", f"{cli}.jsonl"))
            if hits:
                return max(hits, key=os.path.getmtime)
    return None


def find_reply(session_id, marker):
    """Texto final do turno que respondeu ao prompt com `marker`; None se ainda não terminou."""
    path = transcript_for(session_id)
    if not path:
        return None
    with open(path, encoding="utf-8") as f:
        lines = f.read().splitlines()
    start = None
    for i, line in enumerate(lines):
        if marker in line and '"type":"user"' in line.replace(" ", ""):
            start = i
    if start is None:
        return None
    reply, ended = None, False
    for line in lines[start + 1:]:
        try:
            o = json.loads(line)
        except ValueError:
            continue
        msg = o.get("message") or {}
        if o.get("type") == "user" and isinstance(msg.get("content"), str) and "ponte:" in msg["content"]:
            break  # next prompt from the bridge: stop at this turn
        if o.get("type") == "assistant":
            texts = [c.get("text", "") for c in msg.get("content") or [] if c.get("type") == "text"]
            if any(t.strip() for t in texts):
                reply = "\n".join(t for t in texts if t.strip())
            ended = msg.get("stop_reason") == "end_turn"
        elif o.get("type") == "system" and ended:
            return reply
    if ended and reply and time.time() - os.path.getmtime(path) > 8:
        return reply
    return None


# ---------- laço principal ----------

def vigiar():
    os.makedirs(PROMPTS, exist_ok=True)
    try:
        old = int(open(LOCK).read().strip())
        if old != os.getpid() and subprocess.run(["tasklist", "/FI", f"PID eq {old}"], capture_output=True,
                                                 text=True).stdout.count(str(old)):
            emit("ja_rodando", pid=old)
            return
    except (FileNotFoundError, ValueError):
        pass
    with open(LOCK, "w") as f:
        f.write(str(os.getpid()))
    emit("pronto", grupos=[lk["titulo"] for lk in links()])
    emitidos = {}  # lote -> when this watcher last handed it to the Ponte chat
    while True:
        state = load(STATE, {})
        for lk in links():
            g = lk["grupo"]
            st = state.setdefault(g, {"cursor": {"ts": now_iso().replace("T", " "), "id": ""}})
            lote = st.get("lote")
            if lote and lote["status"] == "emitido":
                # never handed over by this watcher (it restarted), or the Ponte chat reported a failure and the
                # retry time has come: hand it over again
                ultimo = emitidos.get(lote["id"])
                retry = lote.get("tentar_apos", 0)
                if ultimo is None or (retry > ultimo and time.time() >= retry):
                    emitidos[lote["id"]] = time.time()
                    emit("entregar", lote=lote["id"], sessao=lk["sessao"], titulo=lk["titulo"], arquivo=lote["arquivo"])
            elif lote and lote["status"] == "entregue":
                reply = find_reply(lk["sessao"], f"ponte:{lote['id']}")
                if reply:
                    if send_group(g, to_whatsapp(reply), st):
                        for mid in lote["mensagens"]:
                            react(g, mid, "✅")
                        st["cursor"] = lote["fim"]
                        st.pop("lote")
                        emit("respondido", lote=lote["id"], titulo=lk["titulo"])
                elif not lote.get("avisou_demora") and time.time() - lote["entregue_em"] > SLOW_NOTICE_S:
                    send_group(g, "Ainda estou trabalhando nisso; mando o retorno assim que terminar.", st)
                    lote["avisou_demora"] = True
            elif not lote:
                msgs = new_messages(g, st["cursor"], set(st.get("enviadas", [])))
                if msgs:
                    for m in msgs:
                        react(g, m["id"], "⏳")
                    lote_id = uuid.uuid4().hex[:8]
                    corpo = "\n".join(describe(g, m) for m in msgs)
                    cab = (f"[via WhatsApp · ponte:{lote_id}] Mensagem do Felipe pelo grupo do WhatsApp ligado a este "
                           "chat. Responda normalmente: a sua resposta final deste turno vai de volta para o WhatsApp "
                           "dele, então seja direto e evite tabelas largas.")
                    if len(msgs) > 1:
                        cab += f"\nSão {len(msgs)} mensagens acumuladas (em ordem):"
                    arquivo = os.path.join(PROMPTS, f"{lote_id}.md")
                    with open(arquivo, "w", encoding="utf-8") as f:
                        f.write(f"{cab}\n\n{corpo}\n")
                    st["lote"] = {"id": lote_id, "status": "emitido", "arquivo": arquivo,
                                  "mensagens": [m["id"] for m in msgs],
                                  "fim": {"ts": msgs[-1]["ts"], "id": msgs[-1]["id"]}}
                    emitidos[lote_id] = time.time()
                    save(STATE, state)
                    emit("entregar", lote=lote_id, sessao=lk["sessao"], titulo=lk["titulo"], arquivo=arquivo)
        save(STATE, state)
        time.sleep(POLL_S)


def set_lote(lote_id, **changes):
    state = load(STATE, {})
    for st in state.values():
        if isinstance(st, dict) and (st.get("lote") or {}).get("id") == lote_id:
            st["lote"].update(changes)
            save(STATE, state)
            return True
    return False


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else ""
    if cmd == "vigiar":
        vigiar()
    elif cmd == "entregue":
        print("ok" if set_lote(sys.argv[2], status="entregue", entregue_em=time.time()) else "lote não encontrado")
    elif cmd == "falhou":
        # stays pending; the watcher hands it over again in a minute (a sleeping chat wakes up on the first try)
        ok = set_lote(sys.argv[2], status="emitido", tentar_apos=time.time() + 60)
        print(f"registrado: {' '.join(sys.argv[3:])}" if ok else "lote não encontrado")
    elif cmd == "ligar":
        grupo, sessao, titulo = sys.argv[2], sys.argv[3], " ".join(sys.argv[4:])
        cfg = load(LINKS, {"links": []})
        cfg["links"] = [lk for lk in cfg["links"] if lk["grupo"] != grupo]
        cfg["links"].append({"grupo": grupo, "sessao": sessao, "titulo": titulo, "desde": now_iso()})
        save(LINKS, cfg)
        state = load(STATE, {})
        state[grupo] = {"cursor": {"ts": now_iso().replace("T", " "), "id": ""}}
        save(STATE, state)
        print(f"ligado: {titulo}")
    elif cmd == "desligar":
        cfg = load(LINKS, {"links": []})
        cfg["links"] = [lk for lk in cfg["links"] if lk["grupo"] != sys.argv[2]]
        save(LINKS, cfg)
        print("desligado")
    elif cmd == "listar":
        print(json.dumps(links(), ensure_ascii=False, indent=2))
    else:
        print(__doc__)


if __name__ == "__main__":
    main()
