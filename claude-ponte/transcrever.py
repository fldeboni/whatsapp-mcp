"""Transcreve um áudio para texto (pt-BR).

Tenta primeiro o Whisper local (faster-whisper, modelo "small", CPU). Se falhar e houver OPENAI_API_KEY
no ambiente, usa a API da OpenAI. Imprime só o texto; sai com código 1 se nada funcionou.

Uso: python transcrever.py <arquivo de áudio>
"""
import json
import os
import sys
import urllib.request
import uuid


def pcm16k(path: str):
    """Decode any audio to 16 kHz mono float32 with ffmpeg (avoids PyAV, whose versions clash with faster-whisper)."""
    import subprocess

    import numpy as np
    raw = subprocess.run(["ffmpeg", "-nostdin", "-loglevel", "error", "-i", path, "-f", "s16le", "-ac", "1",
                          "-ar", "16000", "-"], capture_output=True, check=True).stdout
    return np.frombuffer(raw, np.int16).astype(np.float32) / 32768.0


def local(path: str) -> str:
    from faster_whisper import WhisperModel
    model = WhisperModel(os.environ.get("PONTE_WHISPER_MODEL", "small"), device="cpu", compute_type="int8")
    segments, _ = model.transcribe(pcm16k(path), language="pt", vad_filter=True)
    return " ".join(s.text.strip() for s in segments).strip()


def openai(path: str) -> str:
    key = os.environ.get("OPENAI_API_KEY")
    if not key:
        raise RuntimeError("sem OPENAI_API_KEY")
    boundary = uuid.uuid4().hex
    with open(path, "rb") as f:
        audio = f.read()
    parts = [
        f'--{boundary}\r\nContent-Disposition: form-data; name="model"\r\n\r\ngpt-4o-mini-transcribe\r\n'.encode(),
        f'--{boundary}\r\nContent-Disposition: form-data; name="language"\r\n\r\npt\r\n'.encode(),
        (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{os.path.basename(path)}"\r\n'
         'Content-Type: application/octet-stream\r\n\r\n').encode() + audio + b"\r\n",
        f"--{boundary}--\r\n".encode(),
    ]
    req = urllib.request.Request("https://api.openai.com/v1/audio/transcriptions", data=b"".join(parts),
                                 headers={"Authorization": f"Bearer {key}",
                                          "Content-Type": f"multipart/form-data; boundary={boundary}"})
    with urllib.request.urlopen(req, timeout=300) as r:
        return json.loads(r.read())["text"].strip()


def main() -> int:
    path = sys.argv[1]
    errors = []
    for name, fn in (("local", local), ("openai", openai)):
        try:
            text = fn(path)
            if text:
                sys.stdout.reconfigure(encoding="utf-8")
                print(text)
                return 0
            errors.append(f"{name}: vazio")
        except Exception as e:  # noqa: BLE001 - try the next engine
            errors.append(f"{name}: {e}")
    print("; ".join(errors), file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
