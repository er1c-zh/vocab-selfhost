"""Small local AI gateway for Kokoro TTS, speech-recognition feedback and optional LLM glosses."""

from __future__ import annotations

import base64
from collections import deque
import importlib.util
import io
import json
import math
import os
import re
import sys
import tempfile
import threading
import time
import wave
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.error import HTTPError, URLError
from urllib.parse import parse_qs, urlsplit
from urllib.request import Request, urlopen

MAX_BODY = 16 * 1024 * 1024
_tts_lock = threading.Lock()
_asr_lock = threading.Lock()
_asr_status_lock = threading.Lock()
_phoneme_lock = threading.Lock()
_phoneme_status_lock = threading.Lock()
_tts_inference_lock = threading.Lock()
_asr_inference_lock = threading.Lock()
_phoneme_inference_lock = threading.Lock()
_tts_pipeline: Any = None
_tts_status_lock = threading.Lock()
_tts_status: dict[str, str] = {"state": "idle", "error": ""}
_asr_model: Any = None
_asr_status: dict[str, str] = {"state": "idle", "error": ""}
_phoneme_model: Any = None
_phoneme_status: dict[str, str] = {"state": "idle", "error": ""}
_task_stats_lock = threading.Lock()
_task_stats: dict[str, dict[str, Any]] = {}
_metrics_lock = threading.Lock()
_metrics_previous = (time.monotonic(), time.process_time())
_log_stream: RingLogStream | None = None


class RingLogStream:
    """Tee Python process output to the container and a bounded in-memory log."""

    def __init__(self, stream: Any, max_lines: int = 1000) -> None:
        self.stream = stream
        self.lines: deque[str] = deque(maxlen=max_lines)
        self.pending = ""
        self.lock = threading.Lock()

    def write(self, value: str) -> int:
        if not value:
            return 0
        with self.lock:
            result = self.stream.write(value)
            self.stream.flush()
            self.pending += value
            while "\n" in self.pending:
                line, self.pending = self.pending.split("\n", 1)
                line = line.rstrip("\r")
                if line:
                    self.lines.append(line[-4000:])
            return len(value) if result is None else result

    def flush(self) -> None:
        with self.lock:
            self.stream.flush()

    def snapshot(self, limit: int = 300) -> list[str]:
        with self.lock:
            lines = list(self.lines)
            if self.pending.strip():
                lines.append(self.pending.strip()[-4000:])
            return lines[-max(1, min(limit, 500)):]


def _task_started(name: str) -> None:
    with _task_stats_lock:
        item = _task_stats.setdefault(name, {"running": 0, "requests": 0, "completed": 0, "failed": 0, "totalDurationSeconds": 0.0, "lastDurationSeconds": 0.0})
        item["running"] += 1
        item["requests"] += 1


def _task_finished(name: str, duration: float, succeeded: bool) -> None:
    with _task_stats_lock:
        item = _task_stats.setdefault(name, {"running": 0, "requests": 0, "completed": 0, "failed": 0, "totalDurationSeconds": 0.0, "lastDurationSeconds": 0.0})
        item["running"] = max(0, item["running"] - 1)
        item["completed" if succeeded else "failed"] += 1
        item["totalDurationSeconds"] += duration
        item["lastDurationSeconds"] = duration


def _read_proc_status() -> dict[str, int]:
    result: dict[str, int] = {}
    try:
        for line in Path("/proc/self/status").read_text(encoding="utf-8").splitlines():
            if line.startswith(("VmRSS:", "VmSize:")):
                parts = line.split()
                result[parts[0].rstrip(":")] = int(parts[1]) * 1024
    except (OSError, ValueError, IndexError):
        pass
    return result


def _read_cgroup_memory() -> dict[str, int | None]:
    result: dict[str, int | None] = {"usageBytes": None, "limitBytes": None}
    for usage_path, limit_path in (
        ("/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.max"),
        ("/sys/fs/cgroup/memory/memory.usage_in_bytes", "/sys/fs/cgroup/memory/memory.limit_in_bytes"),
    ):
        try:
            usage = int(Path(usage_path).read_text().strip())
            raw_limit = Path(limit_path).read_text().strip()
            limit = None if raw_limit == "max" else int(raw_limit)
            result = {"usageBytes": usage, "limitBytes": limit}
            break
        except (OSError, ValueError):
            continue
    return result


def metrics_snapshot() -> dict[str, Any]:
    global _metrics_previous
    now_wall = time.monotonic()
    now_cpu = time.process_time()
    with _metrics_lock:
        previous_wall, previous_cpu = _metrics_previous
        _metrics_previous = (now_wall, now_cpu)
    elapsed = now_wall - previous_wall
    cpu_percent = round(max(0.0, (now_cpu - previous_cpu) / elapsed * 100), 1) if elapsed > 0 else None
    status = _read_proc_status()
    with _task_stats_lock:
        tasks = {name: dict(item) for name, item in _task_stats.items()}
    return {
        "process": {"pid": os.getpid(), "rssBytes": status.get("VmRSS"), "virtualBytes": status.get("VmSize"), "cpuPercent": cpu_percent, "cpuSeconds": now_cpu},
        "containerMemory": _read_cgroup_memory(),
        "models": {"tts": tts_model_status(), "asr": asr_model_status(), "pronunciation": pronunciation_model_status(), "asrLoaded": _asr_model is not None, "phonemeScorerLoaded": _phoneme_model is not None},
        "tasks": tasks,
    }


def _clamp(value: float, low: float, high: float) -> float:
    return max(low, min(high, value))


def word_similarity(expected: str, spoken: str) -> float:
    """Return token-level normalized edit similarity; this is not phoneme scoring."""
    left = re.findall(r"[a-z]+(?:'[a-z]+)?", expected.lower())
    right = re.findall(r"[a-z]+(?:'[a-z]+)?", spoken.lower())
    if not left:
        return 0.0
    previous = list(range(len(right) + 1))
    for i, a in enumerate(left, start=1):
        current = [i]
        for j, b in enumerate(right, start=1):
            current.append(min(current[-1] + 1, previous[j] + 1, previous[j - 1] + (a != b)))
        previous = current
    distance = previous[-1]
    return round(_clamp(1 - distance / max(len(left), len(right), 1), 0.0, 1.0), 4)


def pronunciation_result(expected: str, transcript: str, confidence: float) -> dict[str, Any]:
    confidence = round(_clamp(confidence, 0.0, 1.0), 4)
    similarity = word_similarity(expected, transcript)
    if not transcript.strip() or confidence < 0.35:
        status = "inconclusive"
    elif similarity >= 0.8:
        status = "recognized"
    else:
        status = "needs_practice"
    return {
        "expected": expected,
        "transcript": transcript,
        "similarity": similarity,
        "confidence": confidence,
        "status": status,
        "method": "asr-transcript-match",
        "notice": "这是语音识别文本比较反馈，不是音素级发音或口音评分。",
    }


def _load_tts() -> Any:
    global _tts_pipeline
    if _tts_pipeline is not None:
        return _tts_pipeline
    with _tts_lock:
        if _tts_pipeline is None:
            _set_tts_status("loading")
            try:
                from kokoro import KPipeline

                _tts_pipeline = KPipeline(lang_code="a", device="cpu")
            except Exception as exc:
                _set_tts_status("failed", f"{type(exc).__name__}: {exc}")
                raise
            _set_tts_status("ready")
    return _tts_pipeline


def _set_tts_status(state: str, error: str = "") -> None:
    with _tts_status_lock:
        _tts_status.update(state=state, error=error)


def tts_model_status() -> dict[str, str]:
    with _tts_status_lock:
        return dict(_tts_status)


def _preload_models_on_startup() -> None:
    """Load all long-lived speech models once, in sequence, after HTTP starts."""
    loaders = (
        ("Kokoro TTS", _load_tts),
        ("faster-whisper ASR", _load_asr),
        ("Whisper Phoneme CTC", _load_phoneme_scorer),
    )
    for name, loader in loaders:
        started = time.perf_counter()
        try:
            loader()
        except Exception as exc:
            print(
                f"Startup model preload failed: model={name} duration={time.perf_counter() - started:.3f}s "
                f"error={type(exc).__name__}: {exc}",
                flush=True,
            )
        else:
            print(f"Startup model preload ready: model={name} duration={time.perf_counter() - started:.3f}s", flush=True)


def synthesize(text: str, voice: str) -> bytes:
    if len(text) > 1000 or not text.strip():
        raise ValueError("text must contain 1-1000 characters")
    if not re.fullmatch(r"a[fm]_[a-z0-9_]+", voice):
        raise ValueError("voice must be a Kokoro American English voice such as af_heart")
    started = time.perf_counter()
    model_started = time.perf_counter()
    model_load_s = 0.0
    inference_queue_s = 0.0
    inference_s = 0.0
    encode_s = 0.0
    audio_bytes = 0
    status = "error"
    try:
        import numpy as np

        try:
            pipeline = _load_tts()
        finally:
            model_load_s = time.perf_counter() - model_started
        chunks = []
        queue_started = time.perf_counter()
        with _tts_inference_lock:
            inference_started = time.perf_counter()
            inference_queue_s = inference_started - queue_started
            try:
                for _graphemes, _phonemes, audio in pipeline(text, voice=voice, split_pattern=r"\n+"):
                    if audio is not None:
                        chunks.append(np.asarray(audio.detach().cpu().numpy(), dtype=np.float32).reshape(-1))
            finally:
                inference_s = time.perf_counter() - inference_started
        if not chunks:
            raise RuntimeError("Kokoro did not return audio")
        encode_started = time.perf_counter()
        samples = np.clip(np.concatenate(chunks), -1.0, 1.0)
        pcm = (samples * 32767).astype("<i2").tobytes()
        with tempfile.SpooledTemporaryFile() as buffer:
            with wave.open(buffer, "wb") as wav:
                wav.setnchannels(1)
                wav.setsampwidth(2)
                wav.setframerate(24000)
                wav.writeframes(pcm)
            buffer.seek(0)
            result = buffer.read()
        encode_s = time.perf_counter() - encode_started
        audio_bytes = len(result)
        status = "ok"
        return result
    finally:
        print(
            "TTS timing: "
            f"model_load_s={model_load_s:.3f}s "
            f"inference_queue_s={inference_queue_s:.3f}s "
            f"inference_s={inference_s:.3f}s "
            f"encode_s={encode_s:.3f}s "
            f"total_s={time.perf_counter() - started:.3f}s "
            f"chars={len(text)} audio_bytes={audio_bytes} result={status}",
            flush=True,
        )


def _load_asr() -> Any:
    global _asr_model
    if _asr_model is not None:
        return _asr_model
    with _asr_lock:
        if _asr_model is None:
            _set_asr_status("loading")
            try:
                from faster_whisper import WhisperModel

                cache = Path(os.getenv("AI_CACHE_DIR", "/models")) / "whisper"
                cache.mkdir(parents=True, exist_ok=True)
                model_name = os.getenv("ASR_MODEL", "base.en")
                _asr_model = WhisperModel(model_name, device="cpu", compute_type="int8", download_root=str(cache))
            except Exception as exc:
                _set_asr_status("failed", f"{type(exc).__name__}: {exc}")
                raise
            _set_asr_status("ready")
    return _asr_model


def _set_asr_status(state: str, error: str = "") -> None:
    with _asr_status_lock:
        _asr_status.update(state=state, error=error)


def asr_model_status() -> dict[str, str]:
    with _asr_status_lock:
        return dict(_asr_status)


def _load_phoneme_scorer() -> Any:
    global _phoneme_model
    if _phoneme_model is not None:
        return _phoneme_model
    with _phoneme_lock:
        if _phoneme_model is None:
            _set_phoneme_status("downloading")
            try:
                from pronunciation_model import load_phoneme_scorer

                cache = Path(os.getenv("AI_CACHE_DIR", "/models")) / "pronunciation"
                cache.mkdir(parents=True, exist_ok=True)
                _phoneme_model = load_phoneme_scorer(cache)
            except Exception as exc:
                _set_phoneme_status("failed", f"{type(exc).__name__}: {exc}")
                raise
            _set_phoneme_status("ready")
    return _phoneme_model


def _set_phoneme_status(state: str, error: str = "") -> None:
    with _phoneme_status_lock:
        _phoneme_status.update(state=state, error=error)


def pronunciation_model_status() -> dict[str, Any]:
    with _phoneme_status_lock:
        state = dict(_phoneme_status)
    loaded = _phoneme_model is not None
    cached = False
    error = state["error"]
    try:
        from pronunciation_model import cached_phoneme_model_dir

        cache = Path(os.getenv("AI_CACHE_DIR", "/models")) / "pronunciation"
        cached = cached_phoneme_model_dir(cache) is not None
    except Exception as exc:
        if state["state"] == "idle":
            state = {"state": "failed", "error": f"{type(exc).__name__}: {exc}"}
            error = state["error"]

    if state["state"] == "downloading":
        current = "downloading"
    elif state["state"] == "failed":
        current = "failed"
    elif loaded:
        current = "ready"
    elif cached:
        current = "downloaded"
    else:
        current = "not_downloaded"
    return {
        "state": current,
        "downloaded": cached or loaded,
        "loaded": loaded,
        "error": error if current == "failed" else "",
        "asr": asr_model_status(),
        "tts": tts_model_status(),
    }


def _prepare_pronunciation_model() -> None:
    try:
        _load_phoneme_scorer()
    except Exception:
        # The failure is already captured by _load_phoneme_scorer and returned
        # to the settings page through pronunciation_model_status().
        return


def start_pronunciation_model_download() -> dict[str, Any]:
    if _phoneme_model is not None:
        _set_phoneme_status("ready")
        return pronunciation_model_status()
    with _phoneme_status_lock:
        if _phoneme_status["state"] != "downloading":
            _phoneme_status.update(state="downloading", error="")
            threading.Thread(target=_prepare_pronunciation_model, daemon=True).start()
    return pronunciation_model_status()


def _decode_audio_16k_mono(audio: bytes) -> Any:
    """Decode supported browser recordings to the scorer's 16 kHz mono input."""
    import av
    import numpy as np

    chunks = []
    with av.open(io.BytesIO(audio)) as container:
        if not container.streams.audio:
            raise ValueError("recording contains no audio stream")
        resampler = av.AudioResampler(format="fltp", layout="mono", rate=16000)
        for frame in container.decode(audio=0):
            for converted in resampler.resample(frame):
                chunks.append(converted.to_ndarray().reshape(-1))
        for converted in resampler.resample(None):
            chunks.append(converted.to_ndarray().reshape(-1))
    if not chunks:
        raise ValueError("recording contains no decodable audio")
    return np.concatenate(chunks).astype(np.float32, copy=False)


def local_phoneme_result(expected: str, transcript: str, confidence: float, assessment: dict[str, Any]) -> dict[str, Any]:
    accuracy = max(0.0, min(100.0, float(assessment["accuracy"])))
    confidence = round(_clamp(confidence, 0.0, 1.0), 4)
    notice = "本地音素模型将录音与目标词的音素序列对齐评分；分数用于练习反馈，不是口音诊断。"
    if assessment.get("ambiguous"):
        notice += " 多音词按评分较高的词典读音计算。"
    return {
        "expected": expected,
        "transcript": transcript,
        "similarity": round(accuracy / 100.0, 4),
        "confidence": confidence,
        "status": "recognized" if accuracy >= 80 else "needs_practice",
        "method": "local-phoneme-assessment",
        "notice": notice,
        "scores": {"accuracy": round(accuracy, 1)},
        "words": assessment["words"],
    }


def assess_audio(expected: str, audio_b64: str, mime: str) -> dict[str, Any]:
    if not expected.strip() or len(expected) > 100:
        raise ValueError("expected word must contain 1-100 characters")
    try:
        audio = base64.b64decode(audio_b64, validate=True)
    except Exception as exc:
        raise ValueError("audioBase64 is not valid base64") from exc
    if len(audio) < 800:
        return pronunciation_result(expected, "", 0.0)
    if len(audio) > 12 * 1024 * 1024:
        raise ValueError("recording is too large; keep it under 12 MB")

    assessment_started = time.perf_counter()
    timings: dict[str, float] = {}

    def finish(result: dict[str, Any], candidate_count: int = 0) -> dict[str, Any]:
        timings["total_s"] = time.perf_counter() - assessment_started
        details = " ".join(f"{name}={elapsed:.2f}s" for name, elapsed in timings.items())
        print(f"Pronunciation timing: {details} candidates={candidate_count}")
        return result

    suffix = ".webm"
    if "wav" in mime:
        suffix = ".wav"
    elif "mp4" in mime or "m4a" in mime:
        suffix = ".m4a"
    elif "ogg" in mime:
        suffix = ".ogg"
    transcript = ""
    model_confidence = 0.0
    asr_error: Exception | None = None
    asr_started = time.perf_counter()
    asr_inference_started: float | None = None
    try:
        asr_load_started = time.perf_counter()
        model = _load_asr()
        timings["asr_load_s"] = time.perf_counter() - asr_load_started
        with tempfile.NamedTemporaryFile(suffix=suffix) as temp:
            temp.write(audio)
            temp.flush()
            asr_inference_started = time.perf_counter()
            with _asr_inference_lock:
                # Practice recordings contain a single short word. VAD can
                # reject these clips as too brief, so let Whisper inspect the
                # full normalized recording and avoid cross-clip conditioning.
                segments, info = model.transcribe(
                    temp.name,
                    language="en",
                    beam_size=3,
                    vad_filter=False,
                    condition_on_previous_text=False,
                )
                results = list(segments)
            timings["asr_inference_s"] = time.perf_counter() - asr_inference_started
        transcript = " ".join(segment.text.strip() for segment in results).strip()
        if results:
            total = sum(max(segment.end - segment.start, 0.01) for segment in results)
            avg_logprob = sum(segment.avg_logprob * max(segment.end - segment.start, 0.01) for segment in results) / total
            model_confidence = math.exp(min(0.0, avg_logprob)) * float(getattr(info, "language_probability", 1.0))
    except Exception as exc:
        timings.setdefault("asr_load_s", time.perf_counter() - asr_started)
        if asr_inference_started is not None:
            timings.setdefault("asr_inference_s", time.perf_counter() - asr_inference_started)
        else:
            timings.setdefault("asr_inference_s", 0.0)
        # The expected text is enough for phone-level scoring; ASR is only used
        # to display a transcript and should not block the local scorer.
        asr_error = exc
        print(f"Local ASR transcription unavailable: {type(exc).__name__}: {exc}")
    result = pronunciation_result(expected, transcript, model_confidence)
    try:
        from pronunciation_model import PronunciationUnavailable, score_expected_phones

        decode_started = time.perf_counter()
        audio_16k = _decode_audio_16k_mono(audio)
        timings["audio_decode_s"] = time.perf_counter() - decode_started
        phoneme_load_started = time.perf_counter()
        scorer = _load_phoneme_scorer()
        timings["phoneme_model_load_s"] = time.perf_counter() - phoneme_load_started
        phoneme_score_started = time.perf_counter()
        try:
            with _phoneme_inference_lock:
                assessment = score_expected_phones(expected, audio_16k, scorer)
        finally:
            timings["phoneme_score_s"] = time.perf_counter() - phoneme_score_started
        phoneme_result = local_phoneme_result(expected, transcript, model_confidence, assessment)
        if asr_error is not None:
            phoneme_result["notice"] += " faster-whisper 转写暂不可用；本次分数只来自本地音素模型。"
        return finish(phoneme_result, int(assessment.get("candidate_count", 0)))
    except PronunciationUnavailable as exc:
        if transcript:
            result["notice"] = f"{exc} 已回退到本地 ASR 转写匹配；该结果不是音素评分。"
        else:
            result["notice"] = f"{exc} 没有可用的 ASR 转写，本次结果不确定；请重新录音。"
            if asr_error is not None:
                result["notice"] += " faster-whisper 转写也暂不可用。"
    except Exception as exc:
        # Keep transcription feedback usable if a model download, optional
        # dependency, or decoder fails; the response remains explicit.
        print(f"Local phoneme scoring unavailable: {type(exc).__name__}: {exc}")
        if transcript:
            result["notice"] = "本地音素评分暂不可用，已回退到 ASR 转写匹配；该结果不是发音准确度。"
        else:
            result["notice"] = "本地音素评分和 ASR 转写都没有可用结果；本次结果不确定，请重新录音。"
    return finish(result)


def generate_gloss(payload: dict[str, Any]) -> dict[str, Any]:
    # The web UI can save per-deployment text-AI settings in SQLite; the Go API
    # forwards them as a "textAI" object that overrides the process env vars.
    override = payload.get("textAI")
    override = override if isinstance(override, dict) else {}
    provider = str(override.get("provider") or os.getenv("AI_TEXT_PROVIDER", "disabled")).strip().lower()
    if provider != "openai-compatible":
        raise RuntimeError("optional text AI is disabled; enable it in the web settings or set AI_TEXT_PROVIDER=openai-compatible")
    base_url = str(override.get("baseUrl") or os.getenv("AI_TEXT_BASE_URL", "")).strip().rstrip("/")
    model = str(override.get("model") or os.getenv("AI_TEXT_MODEL", "")).strip()
    if not base_url or not model:
        raise RuntimeError("AI text gloss needs a base URL and model name (web settings or AI_TEXT_BASE_URL / AI_TEXT_MODEL)")
    word = str(payload.get("word", "")).strip()
    context = str(payload.get("contextText", "")).strip()
    if not word or len(word) > 100 or len(context) > 5000:
        raise ValueError("word and optional context are outside supported length")
    prompt = (
        "You are helping a Chinese-speaking English learner. "
        "Give a concise learner-friendly definition in BOTH languages: first the English meaning, "
        "then a concise Chinese translation, separated by ' ｜ '. "
        "Explain the word's meaning in the given context in Chinese. "
        "Return only JSON with keys definition (bilingual) and contextMeaning (in Chinese). "
        f"Word: {word}\nSentence: {context or '(not provided)'}"
    )
    body = json.dumps({"model": model, "temperature": 0.2, "messages": [{"role": "user", "content": prompt}]}).encode()
    headers = {"Content-Type": "application/json"}
    api_key = str(override.get("apiKey") or os.getenv("AI_TEXT_API_KEY", "")).strip()
    if api_key:
        headers["Authorization"] = "Bearer " + api_key
    request = Request(base_url + "/chat/completions", data=body, headers=headers, method="POST")
    try:
        with urlopen(request, timeout=90) as response:
            result = json.loads(response.read(2 * 1024 * 1024))
    except (HTTPError, URLError, TimeoutError) as exc:
        raise RuntimeError(f"configured text AI request failed: {exc}") from exc
    try:
        content = result["choices"][0]["message"]["content"].strip()
        if content.startswith("```"):
            content = re.sub(r"^```(?:json)?\s*|\s*```$", "", content, flags=re.IGNORECASE)
        parsed = json.loads(content)
        definition = str(parsed["definition"]).strip()
        context_meaning = str(parsed.get("contextMeaning", "")).strip()
    except (KeyError, IndexError, TypeError, json.JSONDecodeError) as exc:
        raise RuntimeError("configured AI provider did not return the expected JSON fields") from exc
    if not definition:
        raise RuntimeError("configured AI provider returned an empty definition")
    return {"definition": definition, "contextMeaning": context_meaning, "provider": provider}


class Handler(BaseHTTPRequestHandler):
    server_version = "VocabAI/0.1"

    def do_GET(self) -> None:
        endpoint = urlsplit(self.path).path
        if endpoint == "/v1/pronunciation/model":
            self._json(200, pronunciation_model_status())
            return
        if endpoint == "/v1/metrics":
            self._json(200, metrics_snapshot())
            return
        if endpoint == "/v1/logs":
            query = parse_qs(urlsplit(self.path).query)
            try:
                limit = int(query.get("limit", ["300"])[0])
            except ValueError:
                limit = 300
            self._json(200, _log_stream.snapshot(limit) if _log_stream is not None else [])
            return
        if endpoint != "/healthz":
            self._json(404, {"error": "not found"})
            return
        self._json(200, {
            "status": "ok",
            "ttsModel": tts_model_status(),
            "asrModel": asr_model_status(),
            "pronunciationModel": pronunciation_model_status(),
            "features": {"kokoroInstalled": _has_module("kokoro"), "asrInstalled": _has_module("faster_whisper"), "phonemeScorerInstalled": _has_module("torchaudio") and _has_module("whisper"), "textAIEnabled": os.getenv("AI_TEXT_PROVIDER", "disabled") == "openai-compatible"},
        })

    def do_POST(self) -> None:
        endpoint = urlsplit(self.path).path
        task_name = {"/v1/tts": "tts", "/v1/pronunciation": "pronunciation", "/v1/gloss": "gloss", "/v1/pronunciation/model": "pronunciation-model"}.get(endpoint)
        started = time.perf_counter()
        if task_name:
            _task_started(task_name)
        succeeded = False
        try:
            payload = self._read_payload()
            if endpoint == "/v1/tts":
                audio = synthesize(str(payload.get("text", "")), str(payload.get("voice", "af_heart")))
                self._json(200, {"audioBase64": base64.b64encode(audio).decode("ascii"), "format": "wav", "sampleRate": 24000, "provider": "kokoro"})
                succeeded = True
            elif endpoint == "/v1/pronunciation":
                result = assess_audio(str(payload.get("expected", "")), str(payload.get("audioBase64", "")), str(payload.get("audioMime", "")))
                self._json(200, result)
                succeeded = True
            elif endpoint == "/v1/pronunciation/model":
                self._json(202, start_pronunciation_model_download())
                succeeded = True
            elif endpoint == "/v1/gloss":
                self._json(200, generate_gloss(payload))
                succeeded = True
            else:
                self._json(404, {"error": "not found"})
        except ValueError as exc:
            self._json(400, {"error": str(exc)})
        except (ImportError, ModuleNotFoundError) as exc:
            self._json(503, {"error": f"required local model package is unavailable: {exc.name}"})
        except Exception as exc:  # Model load, network and decoder failures remain explicit.
            self._json(503, {"error": f"feature unavailable: {type(exc).__name__}: {exc}"})
        finally:
            if task_name:
                _task_finished(task_name, time.perf_counter() - started, succeeded)

    def _read_payload(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length", "0"))
        if length <= 0 or length > MAX_BODY:
            raise ValueError("request must contain JSON smaller than 16 MB")
        try:
            value = json.loads(self.rfile.read(length))
        except json.JSONDecodeError as exc:
            raise ValueError("invalid JSON") from exc
        if not isinstance(value, dict):
            raise ValueError("request JSON must be an object")
        return value

    def _json(self, status: int, value: dict[str, Any]) -> None:
        data = json.dumps(value, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt: str, *args: Any) -> None:
        # Avoid logging request bodies, model prompts, or recordings.
        print("%s - %s" % (self.address_string(), fmt % args))


def _has_module(name: str) -> bool:
    return importlib.util.find_spec(name) is not None


if __name__ == "__main__":
    address = os.getenv("AI_ADDR", "0.0.0.0")
    port = int(os.getenv("AI_PORT", "8000"))
    _log_stream = RingLogStream(sys.stdout)
    sys.stdout = _log_stream
    sys.stderr = _log_stream
    http_server = ThreadingHTTPServer((address, port), Handler)
    threading.Thread(target=_preload_models_on_startup, name="speech-model-preload", daemon=True).start()
    print(f"Vocab AI service listening on {address}:{port}")
    http_server.serve_forever()

