# oh-my-qqbot Makefile（对齐 REWRITE_PLAN §10 首批落地清单）
GO ?= go
DASHBOARD_DIR := web/dashboard
DIST_DIR := $(DASHBOARD_DIR)/dist
BINARY := oh-my-qqbot.exe

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet run dashboard clean all

all: vet test build

## build: 编译主程序（注入版本号）
build:
	$(GO) build -ldflags "-X oh-my-qqbot/internal.Version=$(VERSION)" -o $(BINARY) ./cmd/bot

## test: 全量测试（-count=1 禁用缓存）
test:
	$(GO) test -count=1 ./internal/...

## vet: 静态检查
vet:
	$(GO) vet ./...

## dashboard: 构建前端（产物进 web/dashboard/dist，供 WebUI 静态托管）
dashboard:
	cd $(DASHBOARD_DIR) && npm install --no-audit --no-fund && npm run build

## run: 本地运行（headless 可加 HEADLESS=1）
run: build
	./$(BINARY) --root .

## clean: 清理构建产物
clean:
	rm -f $(BINARY)
	rm -rf $(DIST_DIR)
