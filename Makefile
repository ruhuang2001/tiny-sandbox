BIN := bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet lint race fmt check-commit install-hooks clean typescript typescript-e2e

build:
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/sandboxd ./cmd/sandboxd
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/sandboxctl ./cmd/sandboxctl

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

fmt:
	gofmt -l -w .

# Validates the last commit's message against type(scope): summary.
check-commit:
	./scripts/check-commit-msg.sh "HEAD~1..HEAD"

# Points git at the tracked hooks once per clone.
install-hooks:
	git config core.hooksPath .githooks

clean:
	rm -rf $(BIN)

typescript:
	cd sandbox-ts && npm ci && npm run build && npm run check

typescript-e2e:
	bash scripts/e2e-typescript.sh artifacts/typescript-e2e
