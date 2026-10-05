FROM python:3.12-slim AS python-deps
ENV PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1
COPY requirements.txt /tmp/requirements.txt
RUN pip install --prefix=/install --extra-index-url https://download.pytorch.org/whl/cpu --no-warn-script-location -r /tmp/requirements.txt
# misaki's English G2P needs the spaCy pipeline model; it is not on PyPI as a
# dependency, so install the wheel matching the spacy version pulled above.
RUN pip install --prefix=/install https://github.com/explosion/spacy-models/releases/download/en_core_web_sm-3.8.0/en_core_web_sm-3.8.0-py3-none-any.whl

FROM python:3.12-slim AS runtime
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 AI_CACHE_DIR=/models HF_HOME=/models/huggingface
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates espeak-ng libgomp1 && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 10002 --shell /usr/sbin/nologin aiuser \
    && mkdir -p /models && chown -R aiuser:aiuser /models
COPY --from=python-deps /install /usr/local
WORKDIR /app
COPY server.py /app/server.py
COPY pronunciation_model.py /app/pronunciation_model.py
COPY PHONEME_MODEL_LICENSE.txt /app/PHONEME_MODEL_LICENSE.txt
USER 10002:10002
EXPOSE 8000
VOLUME ["/models"]
CMD ["python", "/app/server.py"]

