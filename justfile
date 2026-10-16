set shell := ["bash", "-uc"]

config := env_var_or_default("MMXC_CONFIG", "mmxc.yaml")
llm_url := env_var_or_default("MMXC_LLM_BASE_URL", "http://localhost:11434/v1")

# Keep Go's build cache out of the source tree (override with GOCACHE).
export GOCACHE := env_var_or_default("GOCACHE", "/tmp/mmxc-go-cache")

# List the available recipes.
default:
    just --list

# Format the Go sources.
fmt:
    gofmt -w ./cmd ./internal

# Format, then run the tests: the gate before building.
check: fmt
    go test ./...

# Build the standalone binary into bin/mmxc.
build: check
    mkdir -p bin
    go build -buildvcs=false -o bin/mmxc ./cmd/mmxc

# Run from source with the config file (default mmxc.yaml, override with MMXC_CONFIG).
run:
    go run ./cmd/mmxc -config {{config}}

# Build the container image (see docs/DOCKER.md).
docker-build:
    docker build -t mmxc:latest .

# Grade models on assistant tool-calling (see docs/AI-eval.md).
assistant-eval:
    go run ./cmd/assistant-eval --url "{{llm_url}}" --models "${MMXC_ASSISTANT_EVAL_MODELS:-qwen3:30b}" --cases docs/assistant_eval_cases.json

# Score statement extraction against corrected goldens (see docs/AI-eval.md; generate them first).
extraction-eval:
    go run ./cmd/extraction-eval --url "{{llm_url}}" --models "${MMXC_EXTRACTION_EVAL_MODELS:-qwen3:30b}" --statements "${MMXC_EXTRACTION_EVAL_STATEMENTS:-data/samples}" --catalog "${MMXC_EXTRACTION_EVAL_CATALOG:-data/extraction_eval_catalog.json}"

# Write first-draft goldens from one extraction; correct them by hand before scoring.
extraction-eval-generate:
    go run ./cmd/extraction-eval --generate --url "{{llm_url}}" --models "${MMXC_EXTRACTION_EVAL_MODELS:-qwen3:30b}" --statements "${MMXC_EXTRACTION_EVAL_STATEMENTS:-data/samples}" --catalog "${MMXC_EXTRACTION_EVAL_CATALOG:-data/extraction_eval_catalog.json}"

# Remove build output.
clean:
    rm -rf bin
