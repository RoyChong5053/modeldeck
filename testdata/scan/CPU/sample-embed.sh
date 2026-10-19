#!/bin/sh
# fixture for `deck scan` tests: a CPU embed server in a CPU path
LLAMA_BIN=/opt/fake/bin/llama-server
MODEL=/tmp/deck-scan-embed.gguf
exec "$LLAMA_BIN" \
  -m "$MODEL" \
  --embeddings \
  --pooling last \
  --port 22998 \
  -ngl 0
