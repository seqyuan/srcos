# SRCOS build helper.
# The version shown by `srcos --help` is taken from the nearest git tag so it
# always matches the tag (falls back to "dev" outside a git checkout).

VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)

.PHONY: build test vet fmt webui e2e

build:
	go build -ldflags "-X main.version=$(VERSION)" -o srcos .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# 前端包（管理端流程画布），按 ADR-012 的折中方案单独构建：产物输送到
# internal/web/dist/，由 //go:embed 打进同一个二进制。只有画布页加载它，
# 其余页面仍是 Go 模板。
#
# `make build` 不依赖这个目标：internal/web/dist/ 里有一个占位 index.html 进版本库，
# 没有 Node 的环境照样能构建（画布页会提示去跑 make webui）。
webui:
	@if [ -f webui/package.json ]; then \
	  cd webui && (pnpm install --frozen-lockfile || pnpm install) && pnpm build && cd ..; \
	  mkdir -p internal/web/dist; \
	  touch internal/web/dist/.gitkeep; \
	else \
	  echo "webui/ 尚未建立，跳过前端构建"; \
	fi

# 端到端回归网：临时配置 + 临时端口跑通 提交→队列→执行→判定→日志→资源查看。
# 需要本机有 bwrap（或用 SRCOS_E2E_SANDBOX=none）与 curl。
e2e: build
	bash scripts/e2e.sh
