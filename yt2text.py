#!/usr/bin/env python3
"""YouTube video -> text.

1. Tries YouTube's own captions (manual first, then auto-generated).
2. If none exist (or --whisper is given), downloads the audio and
   transcribes it locally with faster-whisper.

Usage:
    python yt2text.py URL [-l ru,en] [-f txt|srt|vtt|json] [-o out.txt]
                          [--whisper] [--model small] [--timestamps]
"""
import argparse
import json
import re
import sys
import tempfile
from pathlib import Path


def video_id(url: str) -> str:
    m = re.search(r"(?:v=|youtu\.be/|shorts/|embed/|live/)([\w-]{11})", url)
    if m:
        return m.group(1)
    if re.fullmatch(r"[\w-]{11}", url):
        return url
    sys.exit(f"Не могу достать ID видео из: {url}")


# ---------- captions ----------
def fetch_captions(vid: str, langs: list[str]):
    from youtube_transcript_api import YouTubeTranscriptApi

    api = YouTubeTranscriptApi()
    tl = api.list(vid)
    try:
        t = tl.find_manually_created_transcript(langs)
    except Exception:
        try:
            t = tl.find_generated_transcript(langs)
        except Exception:
            # any language, translated into the first wanted one if possible
            t = next(iter(tl))
            if t.is_translatable and t.language_code not in langs:
                t = t.translate(langs[0])
    return [
        {"start": s.start, "end": s.start + s.duration, "text": s.text.replace("\n", " ")}
        for s in t.fetch()
    ]


# ---------- whisper ----------
def download_audio(url: str, outdir: Path) -> Path:
    import yt_dlp

    opts = {
        "format": "bestaudio/best",
        "outtmpl": str(outdir / "audio.%(ext)s"),
        "quiet": True,
        "noprogress": True,
        "postprocessors": [
            {"key": "FFmpegExtractAudio", "preferredcodec": "mp3", "preferredquality": "64"}
        ],
    }
    with yt_dlp.YoutubeDL(opts) as ydl:
        ydl.download([url])
    return next(outdir.glob("audio.*"))


def whisper_transcribe(url: str, model: str, lang: str | None):
    from faster_whisper import WhisperModel

    with tempfile.TemporaryDirectory() as d:
        print("Скачиваю аудио...", file=sys.stderr)
        audio = download_audio(url, Path(d))
        print(f"Распознаю (whisper {model})...", file=sys.stderr)
        wm = WhisperModel(model, compute_type="int8")
        segs, info = wm.transcribe(str(audio), language=lang, vad_filter=True, beam_size=5)
        print(f"Язык: {info.language}", file=sys.stderr)
        return [{"start": s.start, "end": s.end, "text": s.text.strip()} for s in segs]


# ---------- formatting ----------
def ts(sec: float, sep: str) -> str:
    h, rem = divmod(int(sec * 1000), 3_600_000)
    m, rem = divmod(rem, 60_000)
    s, ms = divmod(rem, 1000)
    return f"{h:02}:{m:02}:{s:02}{sep}{ms:03}"


def render(segs, fmt: str, timestamps: bool) -> str:
    if fmt == "json":
        return json.dumps(segs, ensure_ascii=False, indent=2)
    if fmt == "srt":
        return "\n".join(
            f"{i}\n{ts(s['start'], ',')} --> {ts(s['end'], ',')}\n{s['text']}\n"
            for i, s in enumerate(segs, 1)
        )
    if fmt == "vtt":
        body = "\n".join(
            f"{ts(s['start'], '.')} --> {ts(s['end'], '.')}\n{s['text']}\n" for s in segs
        )
        return "WEBVTT\n\n" + body
    if timestamps:
        return "\n".join(f"[{ts(s['start'], '.')[:8]}] {s['text']}" for s in segs)
    # plain text: join and split into readable paragraphs
    text = re.sub(r"\s+", " ", " ".join(s["text"] for s in segs)).strip()
    sentences = re.split(r"(?<=[.!?…])\s+", text)
    paras = [" ".join(sentences[i : i + 5]) for i in range(0, len(sentences), 5)]
    return "\n\n".join(paras)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    ap.add_argument("url")
    ap.add_argument("-l", "--lang", default="ru,en", help="приоритет языков, через запятую")
    ap.add_argument("-f", "--format", default="txt", choices=["txt", "srt", "vtt", "json"])
    ap.add_argument("-o", "--output", help="файл вывода (по умолчанию stdout)")
    ap.add_argument("--whisper", action="store_true", help="принудительно использовать Whisper")
    ap.add_argument("--model", default="small", help="tiny/base/small/medium/large-v3")
    ap.add_argument("--timestamps", action="store_true", help="метки времени в txt")
    a = ap.parse_args()

    langs = [x.strip() for x in a.lang.split(",") if x.strip()]
    segs = None
    if not a.whisper:
        try:
            segs = fetch_captions(video_id(a.url), langs)
            print("Взял субтитры YouTube.", file=sys.stderr)
        except Exception as e:
            print(f"Субтитры недоступны ({type(e).__name__}), иду через Whisper.", file=sys.stderr)
    if segs is None:
        segs = whisper_transcribe(a.url, a.model, langs[0] if len(langs) == 1 else None)

    out = render(segs, a.format, a.timestamps)
    if a.output:
        Path(a.output).write_text(out, encoding="utf-8")
        print(f"Сохранено: {a.output}", file=sys.stderr)
    else:
        print(out)


if __name__ == "__main__":
    main()
