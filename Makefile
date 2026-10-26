VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: build test vet fmt run release clean image

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/freedeep ./cmd/freedeep

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

run: build
	./bin/freedeep serve

release:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "building $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" \
			-o dist/freedeep-$$os-$$arch$$ext ./cmd/freedeep || exit 1; \
	done

image:
	podman build --build-arg VERSION=$(VERSION) --tag localhost/freedeep:local --file Containerfile .

clean:
	rm -rf bin dist
