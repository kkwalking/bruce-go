# bruce-go — 单人维护，main 是唯一长期分支。
#
# 版本号的单一来源是 git tag，不再手工修改源码（见 AGENTS.md「版本与发布」）。
# 未打 tag 的工作区用 git describe 的兜底值，构建产物仍能说明自己是什么。

# v0.9.0 -> 0.9.0，tag 之后 3 个提交 -> 0.9.0-3-gabc1234
VERSION ?= $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
LDFLAGS  = -s -w -X bruce-go/internal/version.Current=$(VERSION)
BINARY   = bruce

.PHONY: build install run test race vet fmt fmt-check check sandbox-test clean tag hooks

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
