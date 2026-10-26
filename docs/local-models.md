# Local Models

Drydock calls OpenAI-compatible `/v1/chat/completions` endpoints for every review stage and `/v1/embeddings` for retrieval. This page explains which roles exist, which models to use, and how to serve them with llama.cpp, Ollama or vLLM on your own GPUs. For the end-to-end deployment, see [Deploying Drydock](deployment.md).

## The roles Drydock calls

| Role | Variables | Called for | Needs |
|------|-----------|------------|-------|
| planner | `DRYDOCK_PLANNER_*` | Every review: plans the review and picks a reviewer route. Also generates the walkthrough, conversation replies and codebase chat | Reliable strict JSON, speed |
| coder32b, llm70b, coder14b | `DRYDOCK_CODER32B_*`, `DRYDOCK_LLM70B_*`, `DRYDOCK_CODER14B_*` | The reviewer, on whichever route the planner picks | The best review quality you can afford |
| meta | `DRYDOCK_META_*` | Asynchronous quality control of a sample of reviews | Strict JSON. A stronger model helps |
| agentic discovery | `DRYDOCK_AGENTIC_DISCOVERY_*` (defaults to coder32b) | Agentic path only: explores the repository with tools | Tool calling, fast prompt processing |
| sec70b, secclassify, seclocalize | `DRYDOCK_SEC70B_*` etc. (optional) | Security review and audits ([Security Review](security-review.md)) | Security reasoning |
| embeddings | `DRYDOCK_EMBED_*` | Retrieval (Qdrant) | An embedding model whose dimension matches `DRYDOCK_EMBED_DIMENSION` |

The three reviewer routes are names, not size requirements. They can, and on one GPU should, all point at the same model. On the single-shot path a review makes about **3 calls** (planner, reviewer, walkthrough), plus a JSON repair call whenever a model returns invalid output.

## Which models

A benchmark across 37 model configurations ranked models on drydock's real pipeline and grader. It used hidden planted bugs in real NIP-34 patches, with 3 repetitions each, run at full precision through OpenRouter. The suite and full results are in the nostr-bench repository (`suites/drydock`, `report/drydock-results.md`). The headline:

| Model | Strict recall | Reviews completed | Output tokens / review | Fits |
|-------|---------------|-------------------|------------------------|------|
| **qwen3.8-27b, thinking off** | 57% (50–63) with Drydock's current prompts | 100% | ~1.2k | one 24 GB GPU at Q4_K_M (16.5 GB) |
| qwen3.8-27b, thinking on | 71% (70–73) | 99% | ~19k | same, but very slow on modest GPUs |
| qwen3.6-35b-a3b, thinking off (MoE, 3B active) | 41% | 97% | ~3.7k | one 24 GB GPU at IQ4_XS (17.7 GB); much faster decode |
| qwen3.6-27b, thinking off | 42% | 98% | ~3.7k | one 24 GB GPU |
| gemma-4-26b-a4b (former default) | 17–20% | 100% | ~2k | one 24 GB GPU |
| llama-3.3-70b (former 70B default) | 11% | 95% | ~1k | two 24 GB GPUs |
| qwen2.5-coder-32b (former coder default) | 4% | 43% (fails strict JSON) | ~2k | two 24 GB GPUs |

**Recommendation:** serve **qwen3.8-27b with thinking off** and point every role at it. Keep thinking off unless your GPU decodes fast enough for ~19k tokens per review within your timeout. The built-in defaults in `internal/config` still name the older qwen2.5/llama-3.3 models; set the variables explicitly.

Caveats: these are full-precision results. Q4 quantization usually costs a little accuracy, so measure your own setup with `drydock-bench` ([Evaluation](eval.md#model-benchmarking-drydock-bench)). A frontier-model review of qwen3.8-27b's output found the planted bug in 23/30 cases with correct explanations, but also about 25% false-positive findings. Treat reviews as advice, not verdicts.

## Hardware profiles

| Hardware | Layout |
|----------|--------|
| One 24 GB GPU (RTX 3090/4090, A5000, Tesla P40) | One llama-server with qwen3.8-27b Q4_K_M, thinking off, for every role. Run embeddings on the CPU or in a small second server |
| Two 24 GB GPUs (e.g. 2× Tesla P40) | GPU 0: qwen3.8-27b Q4_K_M for the reviewer routes. GPU 1: a second instance (or qwen3.6-35b-a3b IQ4_XS for speed) for planner, walkthrough and meta, plus embeddings. Or split one Q8_0 copy across both cards for accuracy (measure first) |
| 48–80 GB modern GPU (A6000, L40S, A100, H100) | qwen3.8-27b at Q8_0 or full precision with vLLM. Thinking on becomes practical; enable prefix caching |
| CPU only | Not practical for review latency |

**Tesla P40 and other Pascal cards:** use llama.cpp. vLLM and ExLlama need newer GPUs, and FP16 is very slow on Pascal. Build llama.cpp with `-DCMAKE_CUDA_ARCHITECTURES=61`. Generation speed depends on memory bandwidth, so MoE models with few active parameters decode several times faster than dense 27B models. Prompt processing is slow on Pascal, which makes context size and prompt caching matter.

## Serving with llama.cpp (recommended)

Build (once per host):

```bash
git clone https://github.com/ggml-org/llama.cpp && cd llama.cpp
cmake -B build -DGGML_CUDA=ON              # add -DCMAKE_CUDA_ARCHITECTURES=61 for Pascal (P40)
cmake --build build -j --target llama-server
```

The build needs a recent llama.cpp: `--reasoning` and `--spec-type` must appear in `llama-server --help`.

Chat server for all roles:

```bash
./build/bin/llama-server \
  --hf-repo unsloth/Qwen3.8-27B-GGUF --hf-file Qwen3.8-27B-UD-Q4_K_M.gguf \
  --alias qwen3.8-27b \
  --host 0.0.0.0 --port 8080 \
  -ngl 999 -c 65536 -fa on -ctk q8_0 -ctv q8_0 \
  -np 1 --jinja --reasoning off --metrics --no-webui
```

To require an API key (always, in production), create a key file and add `--api-key-file`:

```bash
sudo mkdir -p /etc/llama && openssl rand -hex 32 | sudo tee /etc/llama/api-keys >/dev/null
sudo chmod 600 /etc/llama/api-keys      # readable by the user running llama-server
# add to the command:  --api-key-file /etc/llama/api-keys
# and set DRYDOCK_LLM_API_KEY to the key in the file
```

| Flag | Why |
|------|-----|
| `--alias qwen3.8-27b` | The model name the server reports. Set every `DRYDOCK_*_MODEL` to exactly this |
| `--host 0.0.0.0` | Lets the Drydock container reach the server via `host.docker.internal`. Restrict access with a firewall, or use the Docker bridge IP instead of `0.0.0.0` |
| `-c 65536` | Context size; see [Context size](#context-size) |
| `-fa on -ctk q8_0 -ctv q8_0` | Flash attention plus an 8-bit KV cache: halves KV memory with negligible quality loss |
| `-np 1` | One request at a time. `-c` is the **total** context shared by all slots, so two 64k slots need `-np 2 -c 131072` (and the memory for it). Raise `DRYDOCK_PIPELINE_WORKERS` to match the slot count |
| `--jinja --reasoning off` | Use the model's chat template with thinking disabled |

To split across two GPUs instead, add `-ts 1,1` (layer split, the default `-sm layer`). To pin a server to one GPU, set `CUDA_VISIBLE_DEVICES=0` (or `1`).

Embedding server, if you enable retrieval:

```bash
./build/bin/llama-server \
  --hf-repo nomic-ai/nomic-embed-text-v1.5-GGUF --hf-file nomic-embed-text-v1.5.Q8_0.gguf \
  --alias nomic-embed-text-v1.5 --embeddings \
  --host 0.0.0.0 --port 8091 -c 8192 -ngl 999
```

Then set `DRYDOCK_EMBED_BASE_URL=http://<host>:8091/v1`, `DRYDOCK_EMBED_MODEL=nomic-embed-text-v1.5` and `DRYDOCK_EMBED_DIMENSION=768`. The dimension must match the model and any existing Qdrant collections. Changing models later means recreating the collections.

Run each server under systemd (or your process supervisor) so it restarts with the host. For TLS in production, add `--ssl-cert-file` and `--ssl-key-file`; see [Production TLS](deployment.md#production-tls).

### Speculative decoding (optional, faster)

Speculative decoding is lossless: it speeds up generation without changing outputs. llama.cpp supports `--spec-type ngram-mod`, which needs no draft model and suits reviews that quote the diff, and `--spec-type draft-mtp` for models published with MTP heads. If accuracy drops with speculation on, that's a serving bug, not a model property. Measure the speed-up with `drydock-bench`.

## Serving with Ollama

Ollama works for development but gives less control:

```bash
OLLAMA_HOST=0.0.0.0:11434 OLLAMA_CONTEXT_LENGTH=65536 OLLAMA_NUM_PARALLEL=1 ollama serve
ollama pull hf.co/unsloth/Qwen3.8-27B-GGUF:Q4_K_M   # use the exact quant tag listed on the model page
ollama pull nomic-embed-text
```

- Set every `DRYDOCK_*_MODEL` to the exact name shown by `ollama list`, and the base URLs to `http://<host>:11434/v1`.
- **Context length:** Ollama's default context is far smaller than Drydock's prompts, so set `OLLAMA_CONTEXT_LENGTH` (or `num_ctx` in a Modelfile). Otherwise prompts are silently truncated and reviews miss the diff.
- **Thinking:** Drydock can't switch thinking off per request through Ollama's OpenAI-compatible endpoint. Check whether your model emits long reasoning and prefer llama.cpp (`--reasoning off`) if it does.
- Ollama has no API-key or TLS support of its own. Front it with a reverse proxy for production.

## Serving with vLLM (Ampere and newer GPUs)

```bash
vllm serve Qwen/Qwen3.8-27B --served-model-name qwen3.8-27b \
  --max-model-len 81920 --enable-prefix-caching --api-key "$KEY" --port 8000
```

- Turn thinking off with the model's chat-template option (`enable_thinking: false`), set as the server's default chat-template kwargs. The flag name varies between vLLM versions.
- vLLM rejects any request whose prompt plus `max_tokens` exceeds `--max-model-len` (HTTP 400). Keep `DRYDOCK_LLM_MAX_TOKENS` at `0`, or small enough to fit.
- Use FP8 or AWQ quantizations to fit smaller GPUs.

## Context size

Drydock assembles a context bundle (the patch, changed-file contents and other context layers) within `DRYDOCK_CONTEXT_TOKEN_BUDGET`, default `64000`. After the 10% tokenizer headroom, that's about **57,600 tokens** of context. Drydock adds system prompts and needs room for the reply. When a prompt exceeds the server's context, that call fails and the review errors. Either serve enough context or lower the budget:

- **Match the budget to your server.** Set `DRYDOCK_CONTEXT_TOKEN_BUDGET` to roughly the served per-slot context minus about 8k for system prompts and the reply. For a 32k server, use `DRYDOCK_CONTEXT_TOKEN_BUDGET=24000` (about 21,600 effective). With llama.cpp `-np N`, the per-slot context is `-c` ÷ N.
- **Or serve more context.** Use 64k or more if memory allows, ideally ~80k with the default budget. Check the KV-cache size llama-server logs at startup, and lower `-c` until weights plus KV fit in VRAM.
- **Typical patches are small.** They need about 5k prompt tokens per call in the benchmark, so the budget mostly matters for large patches.
- **Repositories can only go lower.** A repository can lower the budget further with `context.token_budget` in `.drydock.yaml` ([Per-Repository Configuration](repo-config.md)), but never raise it above the operator setting.
- The agentic path also checks prompts against `DRYDOCK_AGENTIC_MAX_MODEL_CONTEXT` (default 256k). Set it to your server's real context so oversized turns are rejected before they are sent.

## Thinking (reasoning) mode

Thinking models can reason before answering. In the benchmark, thinking on raised qwen3.8-27b's strict recall from about 57% to about 71%, but output grew from about 1.2k to about 19k tokens per review. At 10–30 tokens per second, typical of a dense 27B model on older GPUs, that's 10–30 minutes per review. Keep thinking off unless your GPU is fast. One more risk: in testing, a thinking model on default settings occasionally fell into a degenerate loop of 20k+ tokens. Set a [token cap](#timeouts-and-token-caps) if you enable thinking.

## Timeouts and token caps

- `DRYDOCK_LLM_TIMEOUT` (default `180s`) is the deadline for **one** LLM call. A timed-out call is not retried. Size it as prefill time plus `output tokens ÷ decode speed`. For example, a thinking-off review of ~1.5k output tokens at 15 tok/s plus 30 s of prefill needs about 130 s, so `300s` gives headroom on slow GPUs. Per-role overrides exist (`DRYDOCK_PLANNER_TIMEOUT` etc.).
- `DRYDOCK_LLM_MAX_TOKENS` (default `0`, no cap) bounds runaway generations. Use `8192` with thinking off, `24576` with thinking on. With vLLM, keep prompt plus cap within the context window.
- Details: [Configuration Reference](configuration.md#llm-call-timeout).

## Agentic or single-shot review

Drydock's default review path is **agentic**. A discovery loop explores the repository with tools, and an iterative reviewer verifies findings against evidence. It can find more, but it sends many turns. Benchmarks measured 100k–1M cumulative prompt tokens per review and 3–15 minutes per review even on fast hosted GPUs. On modest local GPUs, start with:

```bash
DRYDOCK_AGENTIC_REVIEW_FALLBACK=true   # single-shot path: ~3 calls per review
```

To use the agentic path:

- Serve a model with reliable tool calling. qwen3.8-27b with thinking on was the most reliable in testing; DeepSeek-class models also work.
- Keep llama.cpp's prompt cache on (the default). Each turn resends the growing conversation, and caching avoids recomputing it.
- Review the loop budgets: `DRYDOCK_AGENTIC_REVIEWER_MAX_TURNS`, `_MAX_TOOL_CALLS` and `_MAX_CUMULATIVE_TOKENS`, and the matching discovery limits ([Configuration Reference](configuration.md#agentic-review)). The cumulative limit re-counts the whole prompt every turn, so long explorations exhaust it quickly.
- Point discovery at a faster model with `DRYDOCK_AGENTIC_DISCOVERY_BASE_URL` and `DRYDOCK_AGENTIC_DISCOVERY_MODEL` if you have one; it defaults to the coder32b endpoint.

## Splitting roles across servers

Each role has its own base URL, model, key, timeout and token cap, so you can place them freely. For example, on two GPUs:

```bash
# GPU 0: reviewer routes
DRYDOCK_CODER32B_BASE_URL=http://gpu-host:8080/v1
DRYDOCK_LLM70B_BASE_URL=http://gpu-host:8080/v1
DRYDOCK_CODER14B_BASE_URL=http://gpu-host:8080/v1
DRYDOCK_CODER32B_MODEL=qwen3.8-27b
DRYDOCK_LLM70B_MODEL=qwen3.8-27b
DRYDOCK_CODER14B_MODEL=qwen3.8-27b
# GPU 1: planner, walkthrough, conversation and meta-review
DRYDOCK_PLANNER_BASE_URL=http://gpu-host:8081/v1
DRYDOCK_META_BASE_URL=http://gpu-host:8081/v1
DRYDOCK_PLANNER_MODEL=qwen3.8-27b
DRYDOCK_META_MODEL=qwen3.8-27b
DRYDOCK_PIPELINE_WORKERS=2
```

Port 8081 here is on the **GPU host**. It doesn't clash with Drydock's management listener unless both run on the same machine; if they do, pick another port.

## Verifying and benchmarking

1. `curl http://<host>:<port>/v1/models` lists the alias you configured.
2. `make eval` (or `drydock-eval`) runs a small labelled dataset through the real pipeline. If it completes, the wiring works.
3. To compare models, quantizations or settings on your hardware, use `drydock-bench` with the nostr-bench `suites/drydock` harness. It reports strict recall, completion rate, JSON repairs, latency per call against your configured timeout, and prefill/decode speeds.
