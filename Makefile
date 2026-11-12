VERSION ?= 0.2.0
PREFIX  ?= $(HOME)/.local
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64

.PHONY: build test install dist clean

build:
	go build -ldflags '$(LDFLAGS)' -o claude-usage-bar .

test:
	go vet ./...
	go test ./...

install: build
	mkdir -p $(PREFIX)/bin
	install -m 0755 claude-usage-bar $(PREFIX)/bin/claude-usage-bar
	$(PREFIX)/bin/claude-usage-bar --install-plugin

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; out=dist/claude-usage-bar-$(VERSION)-$$os-$$arch; \
		echo "$$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$out . || exit 1; \
	done

clean:
	rm -f claude-usage-bar
	rm -rf dist
