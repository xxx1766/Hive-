.PHONY: build test demo clean install-deps examples install uninstall

BIN_DIR := bin
GO      := go
LDFLAGS := -X github.com/anne-x/hive/internal/version.Version=$(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

build: $(BIN_DIR)/hived $(BIN_DIR)/hive $(BIN_DIR)/hive-skill-runner $(BIN_DIR)/hive-workflow-runner examples

$(BIN_DIR)/hived: $(shell find . -name '*.go' -not -path './examples/*')
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $@ ./cmd/hived

$(BIN_DIR)/hive: $(shell find . -name '*.go' -not -path './examples/*')
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $@ ./cmd/hive

$(BIN_DIR)/hive-skill-runner: $(shell find . -name '*.go' -not -path './examples/*')
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $@ ./cmd/hive-skill-runner

$(BIN_DIR)/hive-workflow-runner: $(shell find . -name '*.go' -not -path './examples/*')
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $@ ./cmd/hive-workflow-runner

EXAMPLES := echo fetch upper summarize brief url-summary research memo blob

examples:
	@for agent in $(EXAMPLES); do \
		if [ -f ./examples/$$agent/main.go ]; then \
			mkdir -p ./examples/$$agent/bin; \
			echo "  build example $$agent"; \
			$(GO) build -o ./examples/$$agent/bin/$$agent ./examples/$$agent || exit 1; \
		fi; \
	done

test:
	$(GO) test -race ./...

demo: build
	./scripts/demo.sh

clean:
	rm -rf $(BIN_DIR)

install-deps:
	$(GO) mod tidy

# Installs the four binaries to $(PREFIX)/bin (default /usr/local/bin).
# PREFIX=$$HOME/.local make install  # user-local, no sudo.
PREFIX ?= /usr/local
install: build
	PREFIX=$(PREFIX) ./scripts/install.sh --skip-build

uninstall:
	PREFIX=$(PREFIX) ./scripts/uninstall.sh
