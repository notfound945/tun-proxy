SHELL := /bin/sh

GO ?= go
GOFMT ?= gofmt
INSTALL ?= install
SUDO ?= sudo

PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
CONFIG_DIR ?= $(HOME)/.config/tun-proxy
CONFIG_PATH ?= $(CONFIG_DIR)/config.yaml

BUILD_DIR ?= bin
BINARY := $(BUILD_DIR)/tun-proxy
SYSTEM_BINARY := $(BINDIR)/tun-proxy
GO_FILES := $(shell find cmd internal -type f -name '*.go' -print)

.DEFAULT_GOAL := build
.PHONY: all build build-release clean deps test test-race vet fmt fmt-check check help install system-proxy-check system-proxy-clean

all: build

help:
	@echo 'tun-proxy Makefile targets:'
	@echo '  make build            编译 local 版本到 ./bin/tun-proxy'
	@echo '  make build-release    编译当前 annotated SemVer tag 对应的发布版本'
	@echo '  make test             运行单元测试'
	@echo '  make test-race        运行竞态测试'
	@echo '  make vet              运行 go vet'
	@echo '  make fmt              格式化 Go 源码'
	@echo '  make check            运行格式、测试和 vet 检查'
	@echo '  make install          构建并安装当前 tag 的 release 版本（请勿使用 sudo make）'
	@echo '  make system-proxy-check  只读检查 tun-proxy 是否仍在接管系统网络'
	@echo '  make system-proxy-clean  停止服务并恢复已记录的 DNS/路由状态'
	@echo '  make clean            删除本地编译产物'

deps:
	$(GO) mod download

build:
	mkdir -p "$(BUILD_DIR)"
	$(GO) build -trimpath -ldflags "-X main.version=local" -o "$(BINARY)" ./cmd/tun-proxy

build-release:
	GO="$(GO)" ./scripts/build-release.sh "$(BINARY)"

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GOFMT) -w $(GO_FILES)

fmt-check:
	@files="$$($(GOFMT) -l $(GO_FILES))"; \
	if [ -n "$$files" ]; then \
		echo '以下 Go 文件需要格式化:'; \
		echo "$$files"; \
		exit 1; \
	fi

check: fmt-check test vet

install:
	@if [ "$$(id -u)" -eq 0 ]; then \
		echo '错误: 请以普通用户运行 make install，不要使用 sudo make install。'; \
		exit 1; \
	fi
	$(MAKE) build-release
	$(SUDO) $(INSTALL) -d -m 0755 "$(BINDIR)"
	$(SUDO) $(INSTALL) -m 0755 "$(BINARY)" "$(SYSTEM_BINARY)"
	$(INSTALL) -d -m 0700 "$(CONFIG_DIR)"
	@if [ -e "$(CONFIG_PATH)" ] && [ "$(FORCE_CONFIG)" != '1' ]; then \
		if [ -L "$(CONFIG_PATH)" ] || [ ! -f "$(CONFIG_PATH)" ]; then \
			echo "错误: 配置路径不是普通文件: $(CONFIG_PATH)"; \
			exit 1; \
		fi; \
		chmod 0600 "$(CONFIG_PATH)"; \
		echo "保留现有配置: $(CONFIG_PATH)"; \
	elif [ "$(FORCE_CONFIG)" = '1' ]; then \
		"$(SYSTEM_BINARY)" config -generate -force -config "$(CONFIG_PATH)"; \
	else \
		"$(SYSTEM_BINARY)" config -generate -config "$(CONFIG_PATH)"; \
	fi
	@echo "安装完成: $(SYSTEM_BINARY)"
	@echo "用户配置: $(CONFIG_PATH)"

system-proxy-check: build
	@echo '=== 托管服务状态 ==='
	$(SUDO) "$(BINARY)" service status -json
	@echo '=== 运行与恢复状态 ==='
	$(SUDO) "$(BINARY)" status -json
	@echo '=== 系统网络诊断（只读） ==='
	$(SUDO) "$(BINARY)" diagnose -config "$(CONFIG_PATH)"

system-proxy-clean: build
	@echo '=== 停止 tun-proxy 并回滚运行时系统状态 ==='
	$(SUDO) "$(BINARY)" service stop
	@echo '=== 清理异常退出留下的已记录状态 ==='
	$(SUDO) "$(BINARY)" cleanup
	@echo '=== 清理后托管服务状态 ==='
	$(SUDO) "$(BINARY)" service status -json
	@echo '=== 清理后运行与恢复状态 ==='
	$(SUDO) "$(BINARY)" status

clean:
	rm -f "$(BINARY)"
