# Everything runs inside Docker, so Go does not need to be installed locally.
GO_IMAGE := golang:1.27-alpine
GO := docker run --rm -v "$(CURDIR)":/src -w /src -v rucpy-gomod:/go/pkg/mod -v rucpy-gocache:/root/.cache/go-build $(GO_IMAGE)
VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)

.PHONY: test fmt vet image up down binaries

test:
	$(GO) go test -count=1 ./...

fmt:
	$(GO) gofmt -w .

vet:
	$(GO) go vet ./...

image:
	docker build --build-arg VERSION=$(VERSION) -t rucpy:$(VERSION) -t rucpy:latest .

up:
	docker compose up -d --build

down:
	docker compose down

# Static binaries for Linux, macOS and Windows in ./dist
binaries:
	@for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=$$( [ $$os = windows ] && echo .exe ); \
		echo "building $$os/$$arch"; \
		$(GO) env CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o dist/ruc-$$os-$$arch$$ext ./cmd/ruc || exit 1; \
	done
