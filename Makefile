VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/yoho-build/yoho/internal/cli.Version=$(VERSION)
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

.PHONY: build test vet dist checksums clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o yoho ./cmd/yoho

test:
	go test ./...

vet:
	gofmt -l . | (! grep .) && go vet ./...

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "dist/yoho-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/yoho-$$os-$$arch ./cmd/yoho || exit 1; \
	done

# sha256 lines "<hash>  yoho-<os>-<arch>" (sha256sum on Linux, shasum on macOS).
checksums:
	@mkdir -p dist
	@set -e; : > dist/checksums.txt; \
	for f in dist/yoho-*; do \
		[ -f "$$f" ] || continue; \
		name=$$(basename "$$f"); \
		if command -v sha256sum >/dev/null 2>&1; then \
			hash=$$(sha256sum "$$f" | awk '{print $$1}'); \
		else \
			hash=$$(shasum -a 256 "$$f" | awk '{print $$1}'); \
		fi; \
		printf '%s  %s\n' "$$hash" "$$name" >> dist/checksums.txt; \
	done

clean:
	rm -rf dist yoho
