SHELL := /bin/sh
.NOTPARALLEL:

GO_IMAGE := docker.io/library/golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9
LINT_IMAGE := docker.io/golangci/golangci-lint:v2.15.0@sha256:fbfcfd12a6f8d7f1b97b3362ca80fc2a244d46863148c73cb88e7793af96c481
GO_BUILD_CACHE ?= $(CURDIR)/.cache/build
GO_MOD_CACHE ?= $(CURDIR)/.cache/modules
SQL_IMAGE := localhost/gopgroute-sql-test:pg18.6-pgb1.26.1
RUN = podman run --rm --cpus=2 --memory=2g --pids-limit=256 --cap-drop=ALL --security-opt=no-new-privileges -e GOTOOLCHAIN=go1.27.4+path -e GOFLAGS=-mod=readonly -e GOPROXY=https://proxy.golang.org -e GOSUMDB=sum.golang.org -e GOPRIVATE= -e GONOSUMDB= -e GONOPROXY= -e GOCACHE=/cache -e GOPATH=/modules -v "$(GO_BUILD_CACHE):/cache:z" -v "$(GO_MOD_CACHE):/modules:z" -v "$(CURDIR):/work:z" -w /work $(GO_IMAGE)

# The lint image supplies its own compiler; it does not build runtime artifacts.
LINT_RUN = $(subst GOTOOLCHAIN=go1.27.4+path,GOTOOLCHAIN=local,$(subst $(GO_IMAGE),$(LINT_IMAGE),$(RUN)))

.PHONY: verify cache toolchain tidy build test vet fmt fmt-check lint vulncheck
verify: toolchain tidy build test vet fmt-check lint vulncheck

cache:
	mkdir -p "$(GO_BUILD_CACHE)" "$(GO_MOD_CACHE)"

toolchain: cache
	$(RUN) go version

tidy: cache
	$(RUN) go mod tidy -diff
	$(RUN) go -C tools mod tidy -diff

build: cache
	$(RUN) go build ./...

test: cache
	$(RUN) go test -race -shuffle=on -covermode=atomic -count=1 -timeout=60s ./...

vet: cache
	$(RUN) go vet ./...

fmt: cache
	$(LINT_RUN) golangci-lint fmt

fmt-check: cache
	$(LINT_RUN) golangci-lint fmt --diff

lint: cache
	$(LINT_RUN) golangci-lint run

vulncheck: cache
	$(RUN) go tool -modfile=tools/go.mod govulncheck ./...

.PHONY: sql-image sql-native
sql-image:
	podman build --cpu-period=100000 --cpu-quota=200000 --memory=2g -t $(SQL_IMAGE) -f test/sql/Containerfile test/sql

sql-native: cache sql-image
	mkdir -p .cache
	$(RUN) go test -c -race -cover -tags=sqlnative -o .cache/sql-pooler.test ./internal/pooler
	podman run --rm --network=none --read-only --cpus=2 --memory=2g --pids-limit=256 --cap-drop=ALL --security-opt=no-new-privileges --tmpfs /tmp:rw,nosuid,nodev,size=512m,mode=1777 -v "$(CURDIR)/.cache/sql-pooler.test:/usr/local/bin/pooler.test:ro,z" $(SQL_IMAGE) -test.v -test.shuffle=on -test.timeout=60s
	$(RUN) go test -c -race -cover -tags=sqlnative -o .cache/runtime.test ./internal/app
	podman run --rm --network=none --read-only --cpus=2 --memory=2g --pids-limit=256 --cap-drop=ALL --security-opt=no-new-privileges --tmpfs /tmp:rw,nosuid,nodev,size=512m,mode=1777 -v "$(CURDIR)/.cache/runtime.test:/usr/local/bin/pooler.test:ro,z" $(SQL_IMAGE) -test.v -test.run=TestNativeDaemonSQLRouteLifecycle -test.timeout=60s
