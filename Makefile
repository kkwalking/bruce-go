# bruce-go — 单人维护，main 是唯一长期分支。
#
# 版本号的单一来源是 git tag，不再手工修改源码（见 AGENTS.md「版本与发布」）。
# 未打 tag 的工作区用 git describe 的兜底值，构建产物仍能说明自己是什么。

# v0.9.0 -> 0.9.0，tag 之后 3 个提交 -> 0.9.0-3-gabc1234
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
# 归档名与二进制内注入的版本号都不带 v 前缀。
#
# 单独算一次是因为 `make ... VERSION=v0.9.0` 会绕过上面的 ?=，
# 直接从命令行拿到带 v 的值；不归一化的话，同一个 tag 在本地构建出
# `0.9.0`、在 CI 里构建出 `v0.9.0`。
RELEASE_VERSION = $(patsubst v%,%,$(VERSION))
LDFLAGS  = -s -w -X bruce-go/internal/version.Current=$(RELEASE_VERSION)
BINARY   = bruce

# 发布产物落在 dist/，被 .gitignore 忽略。
DIST      = dist
# 发布只覆盖这两个平台。加平台就在这里加一行。
PLATFORMS = darwin/arm64 linux/amd64
# macOS 没有 sha256sum，BSD 上叫 shasum。
SHA256   ?= $(shell command -v sha256sum >/dev/null 2>&1 && echo sha256sum || echo 'shasum -a 256')

.PHONY: build install run test race vet fmt fmt-check check sandbox-test \
        clean tag release-artifacts hooks

# 新 clone 里跑一次：让 .githooks/ 里的钩子生效。
# 钩子放在仓库内而不是 .git/hooks，才能被 git 跟踪、可复现。
hooks:
	git config core.hooksPath .githooks
	chmod +x .githooks/*
	@echo "hooks installed from .githooks/"
	@echo "  pre-push   拒绝直接推送 main"
	@echo "  commit-msg 要求提交正文（验收报告）"

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/bruce

install:
	go install -trimpath -ldflags="$(LDFLAGS)" ./cmd/bruce

run:
	go run ./cmd/bruce

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

# 提交前跑这一个。
check: fmt-check vet test

# 沙箱集成测试在本机后端不可用时默认跳过；这个目标把跳过升级为失败，
# 用来在本地复现 CI 的严格模式。
sandbox-test:
	BRUCE_REQUIRE_SANDBOX_TESTS=1 go test ./internal/sandbox/ ./internal/mcp/

clean:
	rm -f $(BINARY)
	rm -rf $(DIST)

# 发布产物：每个平台一个 tar.gz，外加 checksums.txt，全部落在 dist/。
#
#   make release-artifacts VERSION=v0.9.0
#
# 归档解开就是二进制本身（没有外层目录），并附一份 README。
# CI 的 release job 调这个目标；本地也能跑，用来在推 tag 之前预演一遍。
#
# CGO_ENABLED=0 是刻意的：linux 产物因此静态链接，不依赖目标机的 glibc 版本；
# darwin 产物也不再依赖 build 机器上的 SDK。代价是 net 包用纯 Go 解析器，
# 行为差异由 internal/web 的测试覆盖。
release-artifacts:
	@rm -rf $(DIST)
	@mkdir -p $(DIST)
	@cp README.md "$(DIST)/README.md"
	@set -eu; \
	for platform in $(PLATFORMS); do \
	  os=$${platform%/*}; arch=$${platform#*/}; \
	  name="$(BINARY)_$(RELEASE_VERSION)_$${os}_$${arch}"; \
	  echo "==> $$name"; \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch \
	    go build -trimpath -ldflags="$(LDFLAGS)" -o "$(DIST)/$(BINARY)" ./cmd/bruce; \
	  tar -czf "$(DIST)/$$name.tar.gz" -C "$(DIST)" $(BINARY) README.md; \
	  rm -f "$(DIST)/$(BINARY)"; \
	done; \
	rm -f "$(DIST)/README.md"; \
	cd "$(DIST)" && $(SHA256) *.tar.gz > checksums.txt; \
	echo "==> dist/"; ls -1 .

# 发布一个新版本：版本号即 tag。
#     make tag VERSION=v0.9.0
# 之后把 tag 推上去，CI 与 git describe 才会看到它。
tag:
	@case "$(VERSION)" in \
	  v[0-9]*.[0-9]*.[0-9]*) ;; \
	  *) echo 'usage: make tag VERSION=v0.9.0'; exit 1 ;; \
	esac
	@git diff --quiet || { echo "working tree is dirty — commit first"; exit 1; }
	@git diff --cached --quiet || { echo "staged changes — commit first"; exit 1; }
	@if git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null; then \
	  echo "$(VERSION) already exists"; exit 1; \
	fi
	git tag -a "$(VERSION)" -m "$(VERSION)"
	@echo "tagged $(VERSION)"
	@echo "push it:  git push origin $(VERSION)"
