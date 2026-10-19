#!/bin/sh
# fixture for `deck scan` tests: a GPU-style chat server
LLAMA_BIN=/opt/fake/bin/llama-server
MODEL=/tmp/deck-scan-fixture.gguf
exec "$LLAMA_BIN" \
  --model "$MODEL" \
  --port 22999 \
  --host 127.0.0.1 \
  -ngl 99 \
  -c 4096 \
  --jinja
