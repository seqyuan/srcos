# SRCOS build helper.
# The version shown by `srcos --help` is taken from the nearest git tag so it
# always matches the tag (falls back to "dev" outside a git checkout).

VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo dev)

.PHONY: build test vet fmt webui e2e e2e-shiny

build:
	@if [ ! -f internal/web/dist/index.html ]; then \
	  printf '\n\033[33m⚠  internal/web/dist/ 没有前端构建产物（缺 index.html）。\033[0m\n'; \
	  echo '   二进制仍可正常构建（画布页会提示去构建）；要包含管理端画布请先跑：make webui'; \
	  printf '\n'; \
	fi
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
# `make build` 不依赖这个目标：internal/web/dist/ 里只有占位的 .gitkeep 进版本库，
# 没有 Node 的环境照样能构建（`make build` 会警告，画布页会提示去跑 make webui）。
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

# Shiny for Python 服务回归（可选项，不在 make e2e 里）。
# 需要一个装了 shiny 的 python：
#   SRCOS_SHINY_PYTHON=/path/to/python make e2e-shiny
# 没有就 SKIP（退出 0），所以在没有 shiny 的机器/CI 上也不会失败。
e2e-shiny: build
	bash scripts/e2e-shiny.sh
