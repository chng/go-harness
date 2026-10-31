# 优先用 PATH 里的 go；没有则回落本机安装路径
GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/sdk/go/bin/go)
BIN := bin/gh

.PHONY: all build ghd test vet fmt fmtcheck tools replay clean

all: fmtcheck vet test build

build:
	@mkdir -p bin
	$(GO) build -trimpath -o $(BIN) ./cmd/gh
	@echo "built $(BIN)"

ghd:
	@mkdir -p bin
	$(GO) build -trimpath -o bin/ghd ./cmd/ghd
	@echo "built bin/ghd（离线 demo: ./bin/ghd --provider mock --addr 127.0.0.1:8088）"

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmtcheck:
	@out=$$($(GO) fmt -n ./... 2>/dev/null | grep -v '^$$' || true); \
	if [ -n "$$out" ]; then echo "需要 gofmt:"; echo "$$out"; exit 1; fi

tools: build
	./$(BIN) tools

replay: build
	@test -n "$(DIR)" || { echo "用法: make replay DIR=~/.go-harness/sessions/xxx"; exit 1; }
	./$(BIN) replay $(DIR)

clean:
	rm -rf bin
