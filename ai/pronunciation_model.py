"""Local phoneme scoring based on vb223/whisper-base-en-phoneme-ctc.

The upstream inference implementation is adapted here so model code is reviewed
and shipped with this service instead of being executed from the model hub.
See PHONEME_MODEL_LICENSE.txt for attribution and license terms.
"""

from __future__ import annotations

import itertools
import json
import os
import re
from functools import lru_cache
from pathlib import Path
from typing import Any


MODEL_ID = "vb223/whisper-base-en-phoneme-ctc"
MODEL_REVISION = "430157adffe84991cb8ca444cf122a4caa6299ff"
MODEL_FILES = ["config.json", "model.safetensors", "gop_regressor.joblib"]
SAMPLE_RATE = 16000
FRAMES_PER_SECOND = 50
GOP_FEATURES = ["gop", "logp_target", "logp_competitor", "logp_target_min", "n_frames"]
MAX_PRONUNCIATION_VARIANTS = 16

_PHONE_REPLACEMENTS = {
    "AX": ["AH0"],
    "AXR": ["ER0"],
    "DX": ["D"],
    "NX": ["N"],
    "EL": ["AH0", "L"],
    "EM": ["AH0", "M"],
    "EN": ["AH0", "N"],
}

_PHONE_IPA = {
    "AA": "ɑ", "AE": "æ", "AH": "ʌ", "AO": "ɔ", "AW": "aʊ", "AY": "aɪ",
    "B": "b", "CH": "tʃ", "D": "d", "DH": "ð", "EH": "ɛ", "ER": "ɝ",
    "EY": "eɪ", "F": "f", "G": "ɡ", "HH": "h", "IH": "ɪ", "IY": "i",
    "JH": "dʒ", "K": "k", "L": "l", "M": "m", "N": "n", "NG": "ŋ",
    "OW": "oʊ", "OY": "ɔɪ", "P": "p", "R": "ɹ", "S": "s", "SH": "ʃ",
    "T": "t", "TH": "θ", "UH": "ʊ", "UW": "u", "V": "v", "W": "w",
    "Y": "j", "Z": "z", "ZH": "ʒ",
}


class PronunciationUnavailable(ValueError):
    """The word or recording could not be scored by the local phoneme model."""


@lru_cache(maxsize=1)
def _cmu_dictionary() -> dict[str, list[list[str]]]:
    """Load the pronunciation dictionary once per AI service process."""
    import cmudict

    return cmudict.dict()


def cached_phoneme_model_dir(cache_dir: str | Path) -> Path | None:
    """Return the pinned snapshot directory when all required files are cached."""
    snapshot = (
        Path(cache_dir)
        / f"models--{MODEL_ID.replace('/', '--')}"
        / "snapshots"
        / MODEL_REVISION
    )
    if all((snapshot / name).is_file() for name in MODEL_FILES):
        return snapshot
    return None


def download_phoneme_model(cache_dir: str | Path) -> Path:
    """Download only the pinned model artifacts and return their snapshot path."""
    from huggingface_hub import snapshot_download

    folder = Path(snapshot_download(
        repo_id=MODEL_ID,
        revision=MODEL_REVISION,
        cache_dir=str(cache_dir),
        allow_patterns=MODEL_FILES,
    ))
    missing = [name for name in MODEL_FILES if not (folder / name).is_file()]
    if missing:
        raise RuntimeError(f"model download is missing required files: {', '.join(missing)}")
    return folder


def _phone_sequence(phones: list[str], valid_phones: set[str]) -> list[str] | None:
    normalized: list[str] = []
    for phone in phones:
        if phone in valid_phones:
            normalized.append(phone)
            continue
        replacement = _PHONE_REPLACEMENTS.get(phone)
        if replacement and all(item in valid_phones for item in replacement):
            normalized.extend(replacement)
            continue
        return None
    return normalized or None


def pronunciation_candidates(
    expected: str,
    valid_phones: set[str],
    dictionary: dict[str, list[list[str]]] | None = None,
) -> list[dict[str, Any]]:
    """Return the available CMUdict pronunciations for each word in a phrase."""
    tokens = re.findall(r"[a-z]+(?:'[a-z]+)?", expected.lower())
    if not tokens:
        raise PronunciationUnavailable("目标词没有可识别的英文发音拼写。")
    if dictionary is None:
        dictionary = _cmu_dictionary()

    variants_by_word: list[list[list[str]]] = []
    for token in tokens:
        variants = dictionary.get(token, [])
        normalized = []
        for variant in variants:
            phones = _phone_sequence(list(variant), valid_phones)
            if phones and phones not in normalized:
                normalized.append(phones)
        if not normalized:
            raise PronunciationUnavailable(f"本地发音词典没有 {token!r} 的兼容读音。")
        variants_by_word.append(normalized[:4])

    candidates: list[dict[str, Any]] = []
    for variants in itertools.islice(itertools.product(*variants_by_word), MAX_PRONUNCIATION_VARIANTS):
        word_phones = [list(phones) for phones in variants]
        candidates.append({
            "words": tokens,
            "word_phones": word_phones,
            "phones": [phone for phones in word_phones for phone in phones],
        })
    return candidates


def _score_value(value: Any) -> float:
    import math

    score = float(value)
    if not math.isfinite(score):
        return 0.0
    return max(0.0, min(2.0, score))


def _ipa(phone: str) -> str:
    base = re.sub(r"[012]$", "", phone)
    symbol = _PHONE_IPA.get(base, base.lower())
    stress = phone[-1] if phone and phone[-1] in "12" and base in _PHONE_IPA else ""
    if stress == "1":
        symbol = "ˈ" + symbol
    elif stress == "2":
        symbol = "ˌ" + symbol
    return f"/{symbol}/"


def score_expected_phones(
    expected: str,
    audio: Any,
    scorer: Any,
    dictionary: dict[str, list[list[str]]] | None = None,
) -> dict[str, Any]:
    """Score all supported dictionary variants and keep the best-fitting one."""
    candidates = pronunciation_candidates(expected, set(scorer.phone_to_id), dictionary)
    prepare_utterance = getattr(scorer, "prepare_utterance", None)
    score_prepared = getattr(scorer, "score_prepared", None)
    prepared = prepare_utterance(audio) if callable(prepare_utterance) and callable(score_prepared) else None
    best: dict[str, Any] | None = None
    for candidate in candidates:
        try:
            rows = (
                score_prepared(prepared, candidate["phones"])
                if prepared is not None
                else scorer.score_utterance(audio, candidate["phones"])
            )
        except ValueError:
            continue
        if len(rows) != len(candidate["phones"]):
            continue
        scores = [_score_value(row.get("score", 0.0)) for row in rows]
        if not scores:
            continue
        mean_score = sum(scores) / len(scores)
        if best is None or mean_score > best["mean_score"]:
            best = {**candidate, "rows": rows, "scores": scores, "mean_score": mean_score}
    if best is None:
        raise PronunciationUnavailable("录音太短或模型未能将语音对齐到目标音素，请再读一次。")

    words = []
    offset = 0
    for word, phones in zip(best["words"], best["word_phones"]):
        phone_rows = best["rows"][offset:offset + len(phones)]
        phone_scores = best["scores"][offset:offset + len(phones)]
        offset += len(phones)
        words.append({
            "word": word,
            "accuracy": round(sum(phone_scores) / len(phone_scores) * 50, 1),
            "phonemes": [
                {"phoneme": _ipa(phone), "accuracy": round(score * 50, 1)}
                for phone, score in zip(phones, phone_scores)
            ],
        })
    return {
        "accuracy": round(best["mean_score"] * 50, 1),
        "words": words,
        "ambiguous": len(candidates) > 1,
        "candidate_count": len(candidates),
    }


def load_phoneme_scorer(cache_dir: str | Path) -> Any:
    """Load the pinned, public checkpoint without executing model-hub Python."""
    import joblib
    import numpy as np
    import torch
    import torchaudio
    import whisper
    from safetensors.torch import load_file
    from whisper.model import AudioEncoder

    folder = download_phoneme_model(cache_dir)
    config = json.loads((folder / "config.json").read_text(encoding="utf-8"))

    class WhisperPhonemeModel(torch.nn.Module):
        def __init__(self, model_config: dict[str, Any]):
            super().__init__()
            self.phone_to_id = {phone: i for i, phone in enumerate(model_config["vocab"])}
            self.output_dim = model_config["output_dim"]
            self.blank_id = model_config["blank_id"]
            self.n_mels = model_config["n_mels"]
            self.max_frames = model_config["n_audio_ctx"]
            self.encoder = AudioEncoder(
                self.n_mels,
                self.max_frames,
                model_config["n_audio_state"],
                model_config["n_audio_head"],
                model_config["n_audio_layer"],
            )
            layers = []
            width = model_config["n_audio_state"]
            for hidden in model_config["head_hidden_sizes"]:
                layers.extend([torch.nn.Linear(width, hidden), torch.nn.GELU(), torch.nn.Dropout(0.1)])
                width = hidden
            layers.append(torch.nn.Linear(width, self.output_dim))
            self.phoneme_head = torch.nn.Sequential(*layers)
            self.regressor = None

        def forward(self, mel: Any) -> Any:
            return self.phoneme_head(self.encoder(mel))

        @torch.no_grad()
        def prepare_utterance(self, audio: Any) -> dict[str, Any]:
            """Run the expensive audio encoder once for all pronunciations."""
            audio_tensor = torch.as_tensor(audio, dtype=torch.float32)
            mel = whisper.log_mel_spectrogram(
                whisper.pad_or_trim(audio_tensor), n_mels=self.n_mels
            )
            device = next(self.parameters()).device
            log_probs = torch.log_softmax(self(mel.unsqueeze(0).to(device)), dim=-1)
            return {"log_probs": log_probs, "audio_samples": len(audio_tensor)}

        @torch.no_grad()
        def score_prepared(self, prepared: dict[str, Any], target_phones: list[str]) -> list[dict[str, Any]]:
            unknown = [phone for phone in target_phones if phone not in self.phone_to_id]
            if unknown:
                raise ValueError(f"unsupported phones: {unknown}")
            if self.regressor is None:
                raise RuntimeError("GOP regressor was not loaded")
            frames = min(
                max(int(prepared["audio_samples"] / SAMPLE_RATE * FRAMES_PER_SECOND), len(target_phones) + 5),
                self.max_frames,
                prepared["log_probs"].shape[1],
            )
            if len(target_phones) >= frames:
                raise ValueError("recording is too short for the target pronunciation")
            log_probs = prepared["log_probs"][:, :frames].float().cpu()
            phone_ids = [self.phone_to_id[phone] for phone in target_phones]
            tokens, frame_scores = torchaudio.functional.forced_align(
                log_probs,
                torch.tensor([phone_ids], dtype=torch.int32),
                torch.tensor([frames], dtype=torch.int32),
                torch.tensor([len(phone_ids)], dtype=torch.int32),
                blank=self.blank_id,
            )
            spans = torchaudio.functional.merge_tokens(
                tokens[0], frame_scores[0], blank=self.blank_id
            )
            rows = []
            for span in spans:
                segment = log_probs[0, span.start:span.end]
                competitors = segment.clone()
                competitors[:, [self.blank_id, span.token]] = float("-inf")
                best_competitor = competitors.max(dim=-1).values
                target = segment[:, span.token]
                rows.append({
                    "phone_id": span.token,
                    "start": span.start,
                    "end": span.end,
                    "gop": float((target - best_competitor).mean()),
                    "logp_target": float(target.mean()),
                    "logp_competitor": float(best_competitor.mean()),
                    "logp_target_min": float(target.min()),
                    "n_frames": span.end - span.start,
                })
            if len(rows) != len(phone_ids):
                raise ValueError("recording could not be aligned to all target phones")
            features = np.array(
                [[row[name] for name in GOP_FEATURES] for row in rows], dtype=np.float32
            )
            onehot = np.zeros((len(rows), self.output_dim), dtype=np.float32)
            onehot[np.arange(len(rows)), [row["phone_id"] for row in rows]] = 1.0
            predictions = self.regressor.predict(np.hstack([features, onehot]))
            return [
                {"phone": phone, "score": float(score),
                 "start": row["start"] / FRAMES_PER_SECOND,
                 "end": row["end"] / FRAMES_PER_SECOND}
                for phone, score, row in zip(target_phones, predictions, rows)
            ]

        @torch.no_grad()
        def score_utterance(self, audio: Any, target_phones: list[str]) -> list[dict[str, Any]]:
            """Compatibility wrapper for callers scoring a single pronunciation."""
            return self.score_prepared(self.prepare_utterance(audio), target_phones)

    model = WhisperPhonemeModel(config)
    model.load_state_dict(load_file(str(folder / "model.safetensors"), device="cpu"), strict=True)
    model.regressor = joblib.load(folder / "gop_regressor.joblib")
    return model.eval()
