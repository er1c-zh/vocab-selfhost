import json
import unittest
import base64
from types import SimpleNamespace
from unittest.mock import patch

from pronunciation_model import PronunciationUnavailable, pronunciation_candidates, score_expected_phones
from server import assess_audio, generate_gloss, local_phoneme_result, pronunciation_result, word_similarity


class PronunciationScoringTests(unittest.TestCase):
    def test_exact_transcript_is_recognized(self):
        result = pronunciation_result("evidence", "evidence", 0.9)
        self.assertEqual(result["status"], "recognized")
        self.assertEqual(result["similarity"], 1.0)
        self.assertEqual(result["method"], "asr-transcript-match")

    def test_empty_or_low_confidence_is_inconclusive(self):
        self.assertEqual(pronunciation_result("word", "", 0.9)["status"], "inconclusive")
        self.assertEqual(pronunciation_result("word", "word", 0.2)["status"], "inconclusive")

    def test_edit_similarity_handles_mismatch(self):
        self.assertEqual(word_similarity("context", "contact"), 0.0)
        self.assertEqual(word_similarity("the result", "the result"), 1.0)

    def test_local_phoneme_result_maps_model_scale_and_status(self):
        result = local_phoneme_result("hello", "hello", 0.82, {"accuracy": 84.5, "words": []})
        self.assertEqual(result["method"], "local-phoneme-assessment")
        self.assertEqual(result["similarity"], 0.845)
        self.assertEqual(result["status"], "recognized")
        self.assertEqual(result["scores"]["accuracy"], 84.5)

    def test_missing_model_falls_back_to_asr_with_notice(self):
        class ASR:
            def __init__(self):
                self.kwargs = None

            def transcribe(self, *_args, **_kwargs):
                self.kwargs = _kwargs
                segment = SimpleNamespace(text="hello", start=0, end=1, avg_logprob=-0.1)
                return iter([segment]), SimpleNamespace(language_probability=0.9)

        asr = ASR()
        payload = {
            "expected": "hello",
            "audioMime": "audio/wav",
            "audioBase64": base64.b64encode(b"a" * 1024).decode("ascii"),
        }
        with patch("server._load_asr", return_value=asr), \
             patch("server._decode_audio_16k_mono", return_value=[0.1, 0.2]), \
             patch("server._load_phoneme_scorer", side_effect=OSError("model download failed")):
            result = assess_audio(payload["expected"], payload["audioBase64"], payload["audioMime"])
        self.assertEqual(result["method"], "asr-transcript-match")
        self.assertIn("回退", result["notice"])
        self.assertFalse(asr.kwargs["vad_filter"], "VAD must not discard short single-word practice clips")
        self.assertFalse(asr.kwargs["condition_on_previous_text"])

    def test_model_status_reports_downloaded_cache(self):
        from server import pronunciation_model_status

        with patch("server._phoneme_model", None), \
             patch("server._phoneme_status", {"state": "idle", "error": ""}), \
             patch("pronunciation_model.cached_phoneme_model_dir", return_value="/models/pinned-snapshot"):
            status = pronunciation_model_status()
        self.assertEqual(status["state"], "downloaded")
        self.assertTrue(status["downloaded"])
        self.assertFalse(status["loaded"])

    def test_phoneme_scoring_runs_when_asr_returns_no_transcript(self):
        class ASR:
            def transcribe(self, *_args, **_kwargs):
                return iter([]), SimpleNamespace(language_probability=0.9)

        payload = {
            "expected": "hello",
            "audioMime": "audio/wav",
            "audioBase64": base64.b64encode(b"a" * 1024).decode("ascii"),
        }
        with patch("server._load_asr", return_value=ASR()), \
             patch("server._decode_audio_16k_mono", return_value=[0.1, 0.2]), \
             patch("server._load_phoneme_scorer", return_value=object()), \
             patch("pronunciation_model.score_expected_phones", return_value={"accuracy": 86, "words": []}):
            result = assess_audio(payload["expected"], payload["audioBase64"], payload["audioMime"])
        self.assertEqual(result["method"], "local-phoneme-assessment")
        self.assertEqual(result["transcript"], "")
        self.assertEqual(result["scores"]["accuracy"], 86)

    def test_local_phoneme_scoring_survives_asr_model_failure(self):
        payload = {
            "expected": "hello",
            "audioMime": "audio/wav",
            "audioBase64": base64.b64encode(b"a" * 1024).decode("ascii"),
        }
        with patch("server._load_asr", side_effect=OSError("ASR model unavailable")), \
             patch("server._decode_audio_16k_mono", return_value=[0.1, 0.2]), \
             patch("server._load_phoneme_scorer", return_value=object()), \
             patch("pronunciation_model.score_expected_phones", return_value={"accuracy": 86, "words": []}):
            result = assess_audio(payload["expected"], payload["audioBase64"], payload["audioMime"])
        self.assertEqual(result["method"], "local-phoneme-assessment")
        self.assertIn("转写暂不可用", result["notice"])


class PhonemeModelAdapterTests(unittest.TestCase):
    def test_candidates_normalize_cmu_phones_and_preserve_alternates(self):
        valid = {"R", "IY1", "EH1", "D", "ER0", "AA1", "K"}
        candidates = pronunciation_candidates(
            "read",
            valid,
            {"read": [["R", "IY1", "D"], ["R", "EH1", "D"]]},
        )
        self.assertEqual(len(candidates), 2)
        self.assertEqual(candidates[0]["phones"], ["R", "IY1", "D"])
        aliases = pronunciation_candidates("word", valid, {"word": [["AXR", "AA1", "DX", "K"]]})
        self.assertEqual(aliases[0]["phones"], ["ER0", "AA1", "D", "K"])

    def test_scorer_chooses_best_homograph_and_groups_phone_results(self):
        class FakeScorer:
            phone_to_id = {phone: index for index, phone in enumerate(["R", "IY1", "EH1", "D"])}

            def score_utterance(self, _audio, phones):
                score = 1.0 if "IY1" in phones else 2.0
                return [{"phone": phone, "score": score} for phone in phones]

        result = score_expected_phones(
            "read",
            [0.0] * 100,
            FakeScorer(),
            {"read": [["R", "IY1", "D"], ["R", "EH1", "D"]]},
        )
        self.assertEqual(result["accuracy"], 100.0)
        self.assertTrue(result["ambiguous"])
        self.assertEqual(result["words"][0]["word"], "read")
        self.assertEqual(result["words"][0]["phonemes"][1], {"phoneme": "/ˈɛ/", "accuracy": 100.0})

    def test_pronunciation_variants_reuse_one_acoustic_encoding(self):
        class PreparedScorer:
            phone_to_id = {phone: index for index, phone in enumerate(["R", "IY1", "EH1", "D"])}

            def __init__(self):
                self.prepare_calls = 0
                self.align_calls = 0

            def prepare_utterance(self, _audio):
                self.prepare_calls += 1
                return "encoded-audio"

            def score_prepared(self, prepared, phones):
                self.assert_prepared(prepared)
                self.align_calls += 1
                score = 1.0 if "IY1" in phones else 2.0
                return [{"phone": phone, "score": score} for phone in phones]

            @staticmethod
            def assert_prepared(prepared):
                if prepared != "encoded-audio":
                    raise AssertionError("expected a shared encoded utterance")

            def score_utterance(self, *_args):
                raise AssertionError("candidate scoring must use the prepared encoding")

        scorer = PreparedScorer()
        result = score_expected_phones(
            "read",
            [0.0] * 100,
            scorer,
            {"read": [["R", "IY1", "D"], ["R", "EH1", "D"]]},
        )
        self.assertEqual(scorer.prepare_calls, 1)
        self.assertEqual(scorer.align_calls, 2)
        self.assertEqual(result["candidate_count"], 2)

    def test_word_without_compatible_pronunciation_is_explicitly_unavailable(self):
        with self.assertRaises(PronunciationUnavailable):
            pronunciation_candidates("notindictionary", {"HH", "AH0"}, {})


class GlossConfigTests(unittest.TestCase):
    def _fake_completion(self, capture):
        class Response:
            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def read(self, _limit):
                return json.dumps(
                    {"choices": [{"message": {"content": json.dumps({"definition": "a lucky find", "contextMeaning": "here: luck"})}}]}
                ).encode()

        def handler(request, timeout):
            capture["url"] = request.full_url
            capture["auth"] = request.headers.get("Authorization")
            body = json.loads(request.data)
            capture["model"] = body["model"]
            capture["prompt"] = body["messages"][0]["content"]
            return Response()

        return handler

    def test_payload_override_enables_gloss_despite_disabled_env(self):
        capture = {}
        with patch.dict("os.environ", {"AI_TEXT_PROVIDER": "disabled"}), patch("server.urlopen", self._fake_completion(capture)):
            result = generate_gloss({
                "word": "serendipity",
                "textAI": {"provider": "openai-compatible", "baseUrl": "http://ollama:11434/v1", "model": "qwen2.5:3b", "apiKey": "k1"},
            })
        self.assertEqual(result["definition"], "a lucky find")
        self.assertTrue(capture["url"].startswith("http://ollama:11434/v1/chat/completions"))
        self.assertEqual(capture["model"], "qwen2.5:3b")
        self.assertEqual(capture["auth"], "Bearer k1")

    def test_payload_disable_wins_over_enabled_env(self):
        with patch.dict("os.environ", {"AI_TEXT_PROVIDER": "openai-compatible", "AI_TEXT_BASE_URL": "http://x/v1", "AI_TEXT_MODEL": "m"}):
            with self.assertRaisesRegex(RuntimeError, "disabled"):
                generate_gloss({"word": "word", "textAI": {"provider": "disabled"}})

    def test_env_still_works_without_override(self):
        capture = {}
        env = {"AI_TEXT_PROVIDER": "openai-compatible", "AI_TEXT_BASE_URL": "http://env-host/v1", "AI_TEXT_MODEL": "env-model"}
        with patch.dict("os.environ", env), patch("server.urlopen", self._fake_completion(capture)):
            result = generate_gloss({"word": "word"})
        self.assertEqual(result["definition"], "a lucky find")
        self.assertEqual(capture["model"], "env-model")

    def test_prompt_requests_bilingual_definition(self):
        capture = {}
        env = {"AI_TEXT_PROVIDER": "openai-compatible", "AI_TEXT_BASE_URL": "http://env-host/v1", "AI_TEXT_MODEL": "env-model"}
        with patch.dict("os.environ", env), patch("server.urlopen", self._fake_completion(capture)):
            generate_gloss({"word": "momentum", "contextText": "Momentum carried the reform forward."})
        prompt = capture["prompt"]
        self.assertIn("English meaning", prompt)
        self.assertIn("Chinese", prompt)
        self.assertIn("Momentum carried the reform forward.", prompt)

    def test_missing_base_url_fails_explicitly(self):
        with patch.dict("os.environ", {"AI_TEXT_PROVIDER": "openai-compatible"}):
            with self.assertRaisesRegex(RuntimeError, "base URL"):
                generate_gloss({"word": "word", "textAI": {"provider": "openai-compatible", "model": "m"}})


if __name__ == "__main__":
    unittest.main()

