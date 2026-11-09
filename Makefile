BIN := bin
PYTHON ?= python3
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: build test vet lint race fmt check-commit install-hooks python-build python-e2e clean

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

# Requires the build frontend; produces local verification artifacts only.
python-build:
	$(PYTHON) -m build --outdir artifacts/python-dist sandbox-python

# Requires Docker; creates a virtual environment unless SDK_INSTALLED=1.
python-e2e:
	PYTHON=$(PYTHON) bash scripts/e2e-python.sh artifacts/python-e2e

clean:
	rm -rf $(BIN)
