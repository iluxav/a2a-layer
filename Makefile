# a2a-layer — build, run, install.
#
#   make build       build ./a2a-layer
#   make run         build, then serve the agents in CONFIG
#   make install     build, then copy a2a-layer into BINDIR (default /usr/local/bin)
#   make uninstall   remove it from BINDIR
#   make test        vet and test
#   make clean       remove the built binary
#
# Override the config or the destination:
#   make run CONFIG=my-agents.yaml
#   make install BINDIR=$$HOME/.local/bin
#
# sudo is used only when BINDIR is not writable by you.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= /usr/local
BINDIR  ?= $(PREFIX)/bin
CONFIG  ?= examples/delegent-team.yaml
BIN     := a2a-layer

.PHONY: build run install uninstall test clean

build:
	go build -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN) ./cmd/a2a-layer

run: build
	./$(BIN) -config "$(CONFIG)"

install: build
	@mkdir -p "$(BINDIR)" 2>/dev/null || true
	@if [ -w "$(BINDIR)" ]; then \
		install -m 0755 $(BIN) "$(BINDIR)/"; \
	else \
		echo "$(BINDIR) is not writable by $$(id -un); using sudo"; \
		sudo mkdir -p "$(BINDIR)" && sudo install -m 0755 $(BIN) "$(BINDIR)/"; \
	fi
	@echo "installed $(BIN) $(VERSION) to $(BINDIR)"
	@case ":$$PATH:" in \
		*":$(BINDIR):"*) ;; \
		*) echo "note: $(BINDIR) is not on your PATH";; \
	esac

uninstall:
	@if [ ! -e "$(BINDIR)/$(BIN)" ]; then \
		echo "$(BIN) is not installed in $(BINDIR)"; \
	elif [ -w "$(BINDIR)" ]; then \
		rm -f "$(BINDIR)/$(BIN)" && echo "removed $(BINDIR)/$(BIN)"; \
	else \
		echo "$(BINDIR) is not writable by $$(id -un); using sudo"; \
		sudo rm -f "$(BINDIR)/$(BIN)" && echo "removed $(BINDIR)/$(BIN)"; \
	fi

test:
	go vet ./... && go test ./...

clean:
	rm -f $(BIN)
