# disk-hog — see README.md. Pure Go, no third-party dependencies, no cgo:
# every target builds offline and cross-compiles from any machine.

APP     := disk-hog
PKG     := ./cmd/$(APP)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
GO      ?= $(shell command -v go 2>/dev/null || echo /usr/local/go/bin/go)
export CGO_ENABLED = 0
export GOTOOLCHAIN = local

# Every platform `make dist` produces. Linux is the main target, so it gets
# every architecture Go supports; macOS, Windows and FreeBSD are extras.
PLATFORMS := linux/amd64 linux/arm64 linux/arm/7 linux/arm/6 linux/386 linux/riscv64 \
             linux/ppc64le linux/s390x linux/loong64 linux/mips64le \
             darwin/amd64 darwin/arm64 windows/amd64 windows/arm64 freebsd/amd64 freebsd/arm64

.PHONY: all build run test race vet fmt lint deploy uninstall dist clean size help

all: build

## build: compile build/disk-hog for this machine
build:
	@mkdir -p build
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o build/$(APP) $(PKG)
	@echo "built build/$(APP) $(VERSION)"

## run: build and serve in the foreground with debug logs
run: build
	build/$(APP) serve --log-level debug

## test: run the test suite
test:
	$(GO) test ./...

## race: run the tests with the race detector (needs cgo)
race:
	CGO_ENABLED=1 $(GO) test -race ./...

## vet: static checks
vet:
	$(GO) vet ./...

## fmt: format the sources
fmt:
	gofmt -w .

## lint: formatting check, vet and tests
lint:
	@test -z "$$(gofmt -l .)" || { echo "unformatted:"; gofmt -l .; exit 1; }
	$(GO) vet ./...
	$(GO) test ./...

## deploy: pull, build, install and (re)start the service (same as ./deploy.sh)
deploy:
	./deploy.sh

## uninstall: stop the service and remove the binary (keeps config)
uninstall:
	./deploy.sh uninstall

## dist: cross-compile release archives for every platform into dist/
dist:
	@rm -rf dist && mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%%/*}; rest=$${p#*/}; arch=$${rest%%/*}; arm=$${rest#*/}; [ "$$arm" = "$$rest" ] && arm=""; \
		name=$(APP)-$$os-$$arch$${arm:+v$$arm}; ext=""; [ $$os = windows ] && ext=.exe; \
		GOOS=$$os GOARCH=$$arch GOARM=$$arm $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o dist/$$name$$ext $(PKG) || exit 1; \
		stage=dist/stage/$(APP); rm -rf dist/stage; mkdir -p $$stage/packaging; \
		cp dist/$$name$$ext $$stage/$(APP)$$ext; cp README.md config.example.json $$stage/; \
		if [ $$os = windows ]; then cp deploy.ps1 $$stage/; \
		else cp deploy.sh $$stage/; cp packaging/* $$stage/packaging/; fi; \
		tar -C dist/stage -czf dist/$$name.tar.gz $(APP); \
		[ "$$arm" = 7 ] && cp dist/$$name$$ext dist/$(APP)-$$os-$$arch; \
		echo "  dist/$$name.tar.gz"; \
	done; rm -rf dist/stage
	@cd dist && (sha256sum *.tar.gz 2>/dev/null || shasum -a 256 *.tar.gz) > SHA256SUMS

## size: show the binary size
size: build
	@ls -lh build/$(APP) | awk '{print $$5"\t"$$9}'

## clean: remove build output
clean:
	rm -rf build dist

## help: list targets
help:
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
