# SRCOS build helper.
# The version shown by `srcos --help` is taken from the nearest git tag so it
# always matches the tag (falls back to "dev" outside a git checkout).

VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)

.PHONY: build test vet fmt webui

build:
	go build -ldflags "-X main.version=$(VERSION)" -o srcos .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# 前端包（文件预览 + 流程 DAG 编辑器），按 ADR-012 的折中方案单独构建，
# 产物输送到 internal/web/dist/ 由 //go:embed 打进同一个二进制。
# webui/ 于 Phase 5 建立；在此之前此目标只给出提示，不阻断构建。
webui:
	@if [ -f webui/package.json ]; then \
	  cd webui && pnpm install --frozen-lockfile && pnpm build; \
	else \
	  echo "webui/ 尚未建立（Phase 5），跳过前端构建"; \
	fi
